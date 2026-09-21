#!/usr/bin/env bash
#
# G3 — docs/31 Gate I: the canonical MOF workflow, end to end.
#
# docs/31_MASTER_ACCEPTANCE.md, Gate I, verbatim:
#
#   完整 MOF workflow 从 Research Question 到外部项目贡献 Evidence 与
#   Profile credit，全程真实 DB/Git/blob/browser，非 mock 演示。
#
# docs/67_TEST_GATES.md asks the same of every phase milestone and makes this
# file the terminal one (P12 必须完整跑通). The chain it names is
#
#   Q → H/branch → experiment → calculation → evidence → claim → finding
#     → PR → main → release → assets → external project evidence
#     → profile credit
#
# and this script walks it. What is *not* in the chain matters as much as what
# is: nothing here is a fixture the gate wrote into a table. The project is
# built by T1201's seed builder (`ops/seed-demo.sh`, which drives the real
# product API over the real PostgreSQL / Gitea / MinIO / Redis), the gate's
# own services are the real `cmd/api` and `cmd/worker`, the web app is the
# built app under `next start`, and the browser is real Chromium.
#
# ---------------------------------------------------------------------------
# What this gate asks that the sibling G3 jobs do not
#
# auth/rsg/gitea -real-services each drive one surface from an EMPTY database:
# they build the world they measure. This one starts from the seed and reaches
# for something else — is the completed workflow still whole end to end, and
# does the END of the chain (external evidence, profile credit) actually
# receive anything. So it does not create the world it measures; it reads back
# the one the builder made. It then adds exactly one proposal of its own and
# drives THAT through review and merge in a browser, because a state
# transition the gate did not watch happen is not evidence that state
# transitions work.
#
# A gate that merely reads is not a gate, so one hop IS produced by this run:
# the replay proposal. Everything else is asserted against what the seed
# built, and would fail just as loudly if the seed had not built it.
#
# The Git half of that proposal is real as well, and it is the one hop no HTTP
# route makes: a research branch carries content because a scientist pushes it,
# and the provider only merges a ref pair that really carries a commit.
# tests/e2e-pr-flows reaches the same hop through a test-owned endpoint (its
# harness's POST /harness/git/push — "a browser cannot run git"); this gate
# runs the real git CLI against the real Gitea instead, in the browser driver's
# [git] step: a scratch clone, a real commit on top of main, a real push with
# a real credential. Nothing about it is simulated, and the end of it is read
# back from Gitea: the post-merge step below asks the provider what its
# refs/heads/main holds and compares that with the sha the merge reported, so
# "the merge landed in Git" is never taken from the merge's own answer.
#
# ---------------------------------------------------------------------------
# Every step is one name and one assertion
#
# The output names each step `[NN] <name>` and prints exactly one `ok`/`FAIL`
# line for it. A failing step says what was expected and what arrived; the
# script exits non-zero. "It ran and printed no error" is not a result this
# script can produce.
#
# ---------------------------------------------------------------------------
# Reproducibility — what has to be true before this can run
#
# Services (host-native; `docker compose up -d` / `make infra-up`, docs/66):
#
#   PostgreSQL  127.0.0.1:5432   postgres/postgres_dev_pw        (POST_DB_*)
#   Redis       127.0.0.1:6379                                    (POST_REDIS_ADDR)
#   Gitea       127.0.0.1:3000   postadmin/postadmin_dev_pw       (POST_GITEA_*)
#   MinIO       127.0.0.1:9000   post-dev-access/post-dev-secret  (POST_BLOB_*)
#
# Those defaults are exactly what ops/seed-demo.sh and the sibling G3 scripts
# already use, so a machine that can run `make seed-demo` can run this one
# with no variable set at all.
#
# Ports this script binds (override if they are taken):
#
#   POST_G3_MOF_API_PORT   default 18085   the gate's own cmd/api
#   POST_G3_MOF_WEB_PORT   default 31170   the built web app (`next start`)
#
# Tools on PATH: go, psql, curl, python3, git, node, npm, pnpm, ss. The
# browser half reuses tests/e2e-pr-flows' own npm project (playwright 1.55.0,
# Chromium headless shell) and tests/web-smoke's library bootstrap rather than
# growing a second harness.
#
# The database this script owns:
#
#   POST_G3_MOF_DB   default post_mof_demo_canonical_gate
#
# It is dropped and recreated at the start of EVERY run by the seed's own
# `seeddemo reset` (the name contains "demo" so that verb accepts it), so a
# second consecutive run cannot fail on residue — there is nothing left of the
# first run to trip over. It is left in place when the run ends, including
# when it fails: the seeded world is then the evidence. It is the only
# database this script touches.
#
# One step is retried, on purpose and in the open: the external half of the
# seed (`--external=1`, step 6) provisions a Gitea fork through the stack's
# single provisioning worker, and ops/seed-demo.md §Known limitations records
# the race that can answer `503 forks unavailable` when a job left over from an
# earlier run is still in that worker's backoff. The builder is idempotent, so
# the step tries at most three times, prints what every failed attempt said,
# and fails outright when the attempts run out. Nothing else in this script
# retries anything.
#
# ---------------------------------------------------------------------------
# Negative coverage
#
# Two refusals this loop depends on, both asserted here (the wider set is
# T1206's):
#
#   1. someone who is not a member of the project cannot merge its proposal
#      (docs/12: merge authority belongs to the target branch's project; the
#      external group FORKS the project, it does not join it). Attempted in
#      the browser and BEFORE the owner's merge, so "it was already merged"
#      can never be the reason.
#   2. a private branch is not published wholesale: the versions the demo
#      selectively published are discoverable in the knowledge network and the
#      rest of that branch's work is not.
#
# Usage: bash tests/acceptance/mof-canonical-workflow.sh
# Exit:  0 only when every step's assertion passed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

API_PORT="${POST_G3_MOF_API_PORT:-18085}"
WEB_PORT="${POST_G3_MOF_WEB_PORT:-31170}"
API_ADDR="127.0.0.1:${API_PORT}"
API_BASE="http://${API_ADDR}"
WEB_BASE="http://127.0.0.1:${WEB_PORT}"
DB_NAME="${POST_G3_MOF_DB:-post_mof_demo_canonical_gate}"
DB_HOST="${POST_DB_HOST:-127.0.0.1}"
DB_PORT="${POST_DB_PORT:-5432}"
DB_USER="${POST_DB_USER:-postgres}"
DB_PASSWORD="${POST_DB_PASSWORD:-postgres_dev_pw}"
DB_URL="postgres://${DB_USER}:${DB_PASSWORD}@${DB_HOST}:${DB_PORT}/${DB_NAME}?sslmode=disable"
REDIS_ADDR="${POST_REDIS_ADDR:-127.0.0.1:6379}"
GITEA_BASE="${POST_GITEA_BASE_URL:-http://127.0.0.1:3000}"
GITEA_ADMIN_USER="${GITEA_ADMIN_USER:-postadmin}"
GITEA_ADMIN_PASSWORD="${GITEA_ADMIN_PASSWORD:-postadmin_dev_pw}"
PLAN="$ROOT/examples/seed-demo/demo-plan.json"
SCIENTIFIC_ADAPTER_URL="${SCIENTIFIC_ADAPTER_URL:-http://127.0.0.1:19100}"
# The browser driver. Overridable ONLY so the mutation harness
# (mof-canonical-mutation-check.sh) can point the gate at a deliberately
# broken copy and prove the assertions below can fail — see its header. A
# normal run never sets it.
DRIVER="${MOF_CANONICAL_DRIVER:-$ROOT/tests/e2e-pr-flows/mof-canonical-e2e.mjs}"

WORK="$(mktemp -d)"
API_PID=""
WORKER_PID=""
WEB_PID=""

cleanup() {
  for pid in "$WEB_PID" "$WORKER_PID" "$API_PID"; do
    if [ -n "$pid" ]; then
      pkill -P "$pid" 2>/dev/null || true
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

STEP=0
FAILS=0
step() { STEP=$((STEP + 1)); printf '\n[%02d] %s\n' "$STEP" "$*"; }
ok()   { printf 'ok   %s\n' "$*"; }
fail() { FAILS=$((FAILS + 1)); printf 'FAIL %s\n' "$*"; }

assert_eq() { # assert_eq WHAT WANT GOT
  if [ "$2" = "$3" ]; then ok "$1 (got $3)"; else fail "$1: expected $2, got $3"; fi
}
assert_min() { # assert_min WHAT MIN GOT — a count is never asserted by "non-empty"
  if [ -n "$3" ] && [ "$3" -ge "$2" ] 2>/dev/null; then ok "$1 (got $3)"
  else fail "$1: expected at least $2, got ${3:-nothing}"; fi
}

summarise() {
  printf '\n'
  if [ "$FAILS" -gt 0 ]; then
    printf 'G3 mof-canonical: FAILED — %d of %d step(s) failed\n' "$FAILS" "$STEP"
    exit 1
  fi
  printf 'G3 mof-canonical: PASSED — %d step(s), every assertion green\n' "$STEP"
  printf '  the seeded world, left in place for inspection: %s\n' "$DB_NAME"
  exit 0
}
die() { fail "$*"; summarise; }

q() { psql "$DB_URL" -tAc "$1" 2>/dev/null | tr -d '[:space:]'; }

port_open() { (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null && exec 3<&- && exec 3>&-; }

# --- preflight -------------------------------------------------------------
step "infrastructure: PostgreSQL, Redis, Gitea and MinIO are all listening"
MISSING_INFRA=""
port_open "$DB_HOST" "$DB_PORT" || MISSING_INFRA="$MISSING_INFRA PostgreSQL($DB_HOST:$DB_PORT)"
port_open "${REDIS_ADDR%%:*}" "${REDIS_ADDR##*:}" || MISSING_INFRA="$MISSING_INFRA Redis($REDIS_ADDR)"
port_open 127.0.0.1 3000 || MISSING_INFRA="$MISSING_INFRA Gitea(127.0.0.1:3000)"
port_open 127.0.0.1 9000 || MISSING_INFRA="$MISSING_INFRA MinIO(127.0.0.1:9000)"
if [ -z "$MISSING_INFRA" ]; then ok "all four accept connections"
else die "not listening:$MISSING_INFRA — bring the dev stack up first (docker compose up -d, docs/66)"; fi

step "tools: everything this gate drives with is on PATH"
MISSING_TOOLS=""
for t in go psql curl python3 git node npm pnpm ss; do
  command -v "$t" >/dev/null 2>&1 || MISSING_TOOLS="$MISSING_TOOLS $t"
done
if [ -z "$MISSING_TOOLS" ]; then ok "go, psql, curl, python3, git, node, npm, pnpm, ss"
else die "missing from PATH:$MISSING_TOOLS — the browser half needs node/npm/pnpm, and this gate is not allowed to skip it"; fi

step "isolation: the gate's own ports are free (a leak from an earlier run is never silently reused)"
LEAKED=""
for p in "$API_PORT" "$WEB_PORT"; do
  ss -tln 2>/dev/null | grep -qE ":$p[[:space:]]" && LEAKED="$LEAKED $p"
done
if [ -z "$LEAKED" ]; then ok "ports $API_PORT and $WEB_PORT are free"
else die "port(s)$LEAKED already listening — a previous run leaked a server, or another gate is running; kill it and re-run"; fi

step "the worktree this gate grades is the one it found (HEAD, and the tree's own dirty set)"
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY 2>/dev/null || true
HEAD_BEFORE="$(git rev-parse HEAD)"
DIRT_BEFORE="$(git status --porcelain)"
# Not "clean": a Worker's tree carries its own uncommitted delivery, and the
# seed's summary file is written outside the tree precisely so that the
# comparison below is about THIS RUN and nothing else. What is asserted is
# that the run changes nothing; it is re-asserted at the end.
assert_eq "HEAD" "$HEAD_BEFORE" "$(git rev-parse HEAD)"

# --- the seeded world ------------------------------------------------------
step "seed: T1201's builder builds the demo into this gate's own database (--external=0)"
SEED_RC=0
SEED_DEMO_SUMMARY="$WORK/summary-1.json" \
SEED_DEMO_BUILD_REPORT="$WORK/build-1.json" \
SEED_DEMO_VERIFY_REPORT="$WORK/verify-1.json" \
SEED_DEMO_API_LOG="$WORK/seed-api-1.log" \
  bash ops/seed-demo.sh --reset --db "$DB_NAME" --external=0 \
  > "$WORK/seed-1.out" 2> "$WORK/seed-1.log" || SEED_RC=$?
SEED1="$(tail -n 1 "$WORK/seed-1.out" 2>/dev/null)"
if [ "$SEED_RC" = 0 ]; then
  ok "the demo built and verified (build_exit_code=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['build_exit_code'])" "$SEED1" 2>/dev/null || echo '?'), verify_exit_code=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['verify_exit_code'])" "$SEED1" 2>/dev/null || echo '?'))"
else
  die "ops/seed-demo.sh --external=0 exited $SEED_RC: $(tail -3 "$WORK/seed-1.log" | tr '\n' ' ') — nothing below can be measured"
fi

step "seed: the external contribution joins the same database (--external=1)"
# The fork provisions a real Gitea repository through the stack's SINGLE
# provisioning worker, so it is the one stage that depends on that worker's
# own tick; ops/seed-demo.md §Known limitations records the bare 503 it shows
# when the worker is busy with a job left over from an earlier run. The
# builder is idempotent (a re-run reuses what the previous attempt created),
# so the attempt is retried, bounded, and every attempt's own summary line is
# printed. This is not a retry that papers over a failure: the loop stops at
# the first success, and the step fails outright when the attempts run out.
EXTERNAL_OK=0
for attempt in 1 2 3; do
  SEED_RC=0
  SEED_DEMO_SUMMARY="$WORK/summary-2.json" \
  SEED_DEMO_BUILD_REPORT="$WORK/build-2.json" \
  SEED_DEMO_VERIFY_REPORT="$WORK/verify-2.json" \
  SEED_DEMO_API_LOG="$WORK/seed-api-2.log" \
    bash ops/seed-demo.sh --db "$DB_NAME" --external=1 \
    > "$WORK/seed-2.out" 2> "$WORK/seed-2.log" || SEED_RC=$?
  if [ "$SEED_RC" = 0 ]; then EXTERNAL_OK=1; break; fi
  # A failed attempt is printed with the lines that explain it, not with a
  # pointer to a file this run deletes on exit: the whole point of printing
  # them is the reader of a red run.
  printf '     attempt %d exited %d:\n' "$attempt" "$SEED_RC"
  grep -iE 'error|failed|unavailable|refused|timeout' "$WORK/seed-2.log" 2>/dev/null \
    | tail -4 | sed 's/^/       | /' || true
  printf '       | (the producer of those lines stopped the API it started; the next attempt starts it again)\n'
  sleep 10
done
if [ "$EXTERNAL_OK" = 1 ]; then ok "the fork, the external PR and the external evidence are in place (attempt $attempt)"
else die "the external contribution did not build in 3 attempts; the fork could not be provisioned (ops/seed-demo.md §Known limitations records this race; the seed's own build_failures names it)"; fi

step "baseline: the demo project exists, with the research question it started from"
PROJECT_ID="$(q "select id from projects where slug='demo-mof-humidity-separation'")"
if [ -n "$PROJECT_ID" ]; then
  assert_eq "objects of type research_question (the Q the chain starts at)" "1" \
    "$(q "select count(*) from scientific_objects where project_id='$PROJECT_ID' and object_type='research_question'")"
else fail "no project with slug demo-mof-humidity-separation — the seed did not build the demo"; fi

step "baseline: the hypothesis and the branches cut from the accepted state"
MAIN_BRANCH_ID="$(q "select id from branches where project_id='$PROJECT_ID' and name='main'")"
if [ -n "$MAIN_BRANCH_ID" ]; then
  printf '     main=%s, %s hypotheses, %s branches, %s objects, %s state commits\n' \
    "${MAIN_BRANCH_ID:0:8}" \
    "$(q "select count(*) from scientific_objects where project_id='$PROJECT_ID' and object_type='hypothesis'")" \
    "$(q "select count(*) from branches where project_id='$PROJECT_ID'")" \
    "$(q "select count(*) from scientific_objects where project_id='$PROJECT_ID'")" \
    "$(q "select count(*) from state_commits where branch_id='$MAIN_BRANCH_ID'")"
  assert_min "hypotheses the branches explore (H)" 1 \
    "$(q "select count(*) from scientific_objects where project_id='$PROJECT_ID' and object_type='hypothesis'")"
else fail "the demo project has no main branch"; fi

step "baseline: main's accepted state is a real history, not an empty branch"
MAIN_STATE_BEFORE="$(q "select count(*) from state_commits where branch_id='$MAIN_BRANCH_ID'")"
assert_min "state commits on main (each one a research PR merge)" 2 "${MAIN_STATE_BEFORE:-0}"

step "baseline: experiment, calculation, claim and finding are all present (the middle of the chain)"
MIDDLE_MISSING=""
for type in experiment calculation claim finding; do
  n="$(q "select count(*) from scientific_objects where project_id='$PROJECT_ID' and object_type='$type'")"
  [ -n "$n" ] && [ "$n" -ge 1 ] 2>/dev/null || MIDDLE_MISSING="$MIDDLE_MISSING $type"
done
if [ -z "$MIDDLE_MISSING" ]; then
  ok "experiment=$(q "select count(*) from scientific_objects where project_id='$PROJECT_ID' and object_type='experiment'") calculation=$(q "select count(*) from scientific_objects where project_id='$PROJECT_ID' and object_type='calculation'") claim=$(q "select count(*) from scientific_objects where project_id='$PROJECT_ID' and object_type='claim'") finding=$(q "select count(*) from scientific_objects where project_id='$PROJECT_ID' and object_type='finding'")"
else fail "the chain is missing object type(s):$MIDDLE_MISSING"; fi

step "baseline: the proposals the demo's own history turned on"
MERGED_PRS="$(q "select count(*) from pull_requests where project_id='$PROJECT_ID' and state='merged'")"
CONFLICT_PR="$(q "select count(*) from pull_requests pr join branches s on s.id=pr.source_branch_id where s.name='protocol-activation-180c' and pr.state='merge_ready'")"
FORK_ID="$(q "select id from projects where slug like 'demo-mof-humidity-separation-%' limit 1")"
EXTERNAL_PR="$(q "select count(*) from pull_requests pr join branches s on s.id=pr.source_branch_id where s.project_id='$FORK_ID' and pr.state='open'")"
if [ -n "$FORK_ID" ] && [ "${MERGED_PRS:-0}" -ge 2 ] && [ "${CONFLICT_PR:-0}" -ge 1 ] && [ "${EXTERNAL_PR:-0}" -ge 1 ]; then
  ok "$MERGED_PRS merged PR(s), 1 proposal the product REFUSED to merge (the undecided conflict) still waiting, and the external group's proposal open from its own project"
else fail "expected >=2 merged PRs, the refused conflict proposal, and the fork's open proposal — got merged=${MERGED_PRS:-none} conflict=${CONFLICT_PR:-none} fork=${FORK_ID:-none} external_open=${EXTERNAL_PR:-none}"; fi

step "baseline: the releases and the assets the accepted state produced"
RELEASES="$(q "select count(*) from releases where project_id='$PROJECT_ID'")"
ASSETS="$(q "select count(*) from research_assets where origin_project_id='$PROJECT_ID'")"
ASSET_PIDS="$(q "select count(*) from research_assets where origin_project_id='$PROJECT_ID' and coalesce(pid,'')<>''")"
if [ -n "$RELEASES" ] && [ "$RELEASES" -ge 1 ] && [ -n "$ASSETS" ] && [ "$ASSETS" -ge 1 ] && [ "$ASSETS" = "$ASSET_PIDS" ]; then
  ok "$RELEASES release(s), and all $ASSETS assets carry a persistent id (pid) the knowledge network can cite"
else fail "releases=${RELEASES:-none}, assets=${ASSETS:-none} of which ${ASSET_PIDS:-none} carry a pid — expected at least 1 release and every asset with a pid"; fi

step "baseline: the private branch is selectively published, not published wholesale"
PRIV_BRANCH="$(q "select id from branches where project_id='$PROJECT_ID' and visibility='private' limit 1")"
PRIV_PUBLISHED_OBJ="$(q "select o.id from scientific_objects o join scientific_object_versions v on v.object_id=o.id join knowledge_publications kp on kp.object_version_id=v.id where v.branch_id='$PRIV_BRANCH' limit 1")"
PRIV_UNPUBLISHED_OBJ="$(q "select o.id from scientific_objects o join scientific_object_versions v on v.object_id=o.id left join knowledge_publications kp on kp.object_version_id=v.id where v.branch_id='$PRIV_BRANCH' and kp.id is null limit 1")"
if [ -n "$PRIV_BRANCH" ] && [ -n "$PRIV_PUBLISHED_OBJ" ] && [ -n "$PRIV_UNPUBLISHED_OBJ" ]; then
  ok "the branch has both a selectively published version and an unpublished one (the negative below needs both)"
else fail "private branch=${PRIV_BRANCH:-none} published=${PRIV_PUBLISHED_OBJ:-none} unpublished=${PRIV_UNPUBLISHED_OBJ:-none} — selective publication needs the branch to have both"; fi

# --- the gate's own services ----------------------------------------------
step "services: the gate mints its own Gitea service token for the provider actor"
GITEA_TOKEN="$(curl -fsS --max-time 30 -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASSWORD}" \
  -X POST "${GITEA_BASE}/api/v1/users/${GITEA_ADMIN_USER}/tokens" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"mof-canonical-gate-$(date +%s)\",\"scopes\":[\"write:repository\",\"write:user\",\"write:admin\"]}" \
  2>/dev/null | sed -n 's/.*"sha1":"\([^"]*\)".*/\1/p')"
if [ -n "$GITEA_TOKEN" ]; then ok "minted one from Gitea's own API"
else die "Gitea at $GITEA_BASE would not mint a token for ${GITEA_ADMIN_USER} — the fork and provider paths fail closed without one"; fi

# One environment for both processes: cmd/api and cmd/worker read the same
# configuration, and a worker that cannot name the provider projects nothing
# (cmd/worker refuses to start without POST_GITEA_TOKEN).
api_env() {
  env POST_ENV=dev \
    POST_API_ADDR="$API_ADDR" \
    POST_MCP_ADDR="127.0.0.1:$((API_PORT + 1000))" \
    POST_DB_HOST="$DB_HOST" POST_DB_PORT="$DB_PORT" POST_DB_USER="$DB_USER" \
    POST_DB_PASSWORD="$DB_PASSWORD" POST_DB_NAME="$DB_NAME" POST_DB_SSLMODE=disable \
    POST_REDIS_ADDR="$REDIS_ADDR" \
    POST_BLOB_ENDPOINT="${POST_BLOB_ENDPOINT:-http://127.0.0.1:9000}" \
    POST_BLOB_ACCESS_KEY="${POST_BLOB_ACCESS_KEY:-post-dev-access}" \
    POST_BLOB_SECRET_KEY="${POST_BLOB_SECRET_KEY:-post-dev-secret}" \
    POST_BLOB_BUCKET="${POST_BLOB_BUCKET:-post}" POST_BLOB_USE_TLS=false \
    POST_GITEA_TOKEN="$GITEA_TOKEN" POST_GITEA_BASE_URL="$GITEA_BASE" \
    POST_GITEA_WEBHOOK_URL="$API_BASE/api/v1/git/hooks/gitea" \
    POST_WEB_ORIGIN="$WEB_BASE" \
    "$@"
}

go build -o "$WORK/api" ./cmd/api > "$WORK/build-api.log" 2>&1 \
  || die "building cmd/api: $(tail -3 "$WORK/build-api.log" | tr '\n' ' ')"
go build -o "$WORK/worker" ./cmd/worker > "$WORK/build-worker.log" 2>&1 \
  || die "building cmd/worker: $(tail -3 "$WORK/build-worker.log" | tr '\n' ' ')"

step "services: the gate's own real cmd/api answers /healthz on $API_ADDR"
api_env "$WORK/api" > "$WORK/api.log" 2>&1 &
API_PID=$!
HEALTHY=0
for _ in $(seq 1 60); do
  curl -fsS --max-time 2 "$API_BASE/healthz" >/dev/null 2>&1 && { HEALTHY=1; break; }
  kill -0 "$API_PID" 2>/dev/null || break
  sleep 0.5
done
if [ "$HEALTHY" = 1 ]; then ok "the API is up against $DB_NAME, with $WEB_BASE as the browser origin it admits"
else die "the API did not become healthy: $(tail -5 "$WORK/api.log" | tr '\n' ' ')"; fi

step "services: cmd/worker projects the Contribution Ledger onto the external contributor"
EXTERNAL_USER_ID="$(q "select id from users where email='demo.external@example.invalid'")"
LEDGER_BEFORE="$(q "select count(*) from contribution_events where actor_id='$EXTERNAL_USER_ID'")"
api_env "$WORK/worker" > "$WORK/worker.log" 2>&1 &
WORKER_PID=$!
PROJECTED=0
LEDGER_NOW="$LEDGER_BEFORE"
for _ in $(seq 1 60); do
  LEDGER_NOW="$(q "select count(*) from contribution_events where actor_id='$EXTERNAL_USER_ID'")"
  if [ -n "$LEDGER_NOW" ] && [ "$LEDGER_NOW" != "0" ]; then PROJECTED=1; break; fi
  kill -0 "$WORKER_PID" 2>/dev/null || break
  sleep 2
done
if [ "$PROJECTED" = 1 ]; then ok "research_events → contribution_events moved for them (was $LEDGER_BEFORE, now $LEDGER_NOW row(s))"
else fail "the ledger never credited $EXTERNAL_USER_ID: expected >=1 contribution_events row, got ${LEDGER_NOW:-nothing} after 120s (worker log tail: $(tail -3 "$WORK/worker.log" | tr '\n' ' '))"; fi

step "services: the built web app is serving on $WEB_BASE (the API origin baked into it)"
# The app validates its environment at build AND at start (lib/config.ts fails
# closed) and bakes the API origin into the client bundle, so the build and
# the `next start` get the same three variables — as tests/web-smoke and CI do.
if [ ! -d "$ROOT/apps/web/node_modules" ]; then
  printf '     cold tree: installing web dependencies first\n'
  (cd "$ROOT" && pnpm install --frozen-lockfile) > "$WORK/pnpm.log" 2>&1 \
    || die "pnpm install --frozen-lockfile failed: $(tail -3 "$WORK/pnpm.log" | tr '\n' ' ')"
fi
(cd "$ROOT/apps/web" && POST_ENV=prod API_BASE_URL="$API_BASE" SCIENTIFIC_ADAPTER_URL="$SCIENTIFIC_ADAPTER_URL" \
  pnpm run build) > "$WORK/web-build.log" 2>&1 \
  || die "the web app did not build: $(tail -5 "$WORK/web-build.log" | tr '\n' ' ')"
(cd "$ROOT/apps/web" && POST_ENV=prod API_BASE_URL="$API_BASE" SCIENTIFIC_ADAPTER_URL="$SCIENTIFIC_ADAPTER_URL" \
  exec "$ROOT/apps/web/node_modules/.bin/next" start -p "$WEB_PORT") > "$WORK/web.log" 2>&1 &
WEB_PID=$!
WEB_READY=0
for _ in $(seq 1 90); do
  curl -sf -o /dev/null "$WEB_BASE/" && { WEB_READY=1; break; }
  kill -0 "$WEB_PID" 2>/dev/null || break
  sleep 0.5
done
if [ "$WEB_READY" = 1 ]; then ok "the real app is up (built with API_BASE_URL=$API_BASE)"
else die "the web app did not become ready: $(tail -5 "$WORK/web.log" | tr '\n' ' ')"; fi

# --- the browser -----------------------------------------------------------
step "browser: the playwright project in tests/e2e-pr-flows is installed (reused, never copied)"
(cd "$ROOT/tests/e2e-pr-flows" && npm install --no-audit --no-fund) > "$WORK/npm.log" 2>&1 \
  || die "npm install in tests/e2e-pr-flows failed: $(tail -3 "$WORK/npm.log" | tr '\n' ' ')"
(cd "$ROOT/tests/e2e-pr-flows" && PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell) >> "$WORK/npm.log" 2>&1
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1
if [ -d "$ROOT/tests/e2e-pr-flows/node_modules/playwright" ]; then
  ok "the driver will run from $( [ -n "$LIB_PATH" ] && echo 'the bootstrapped Chromium libraries' || echo 'the host libraries' )"
else fail "tests/e2e-pr-flows/node_modules/playwright is absent after npm install"; fi

step "browser: the gate's own proposal is opened, reviewed and merged in real Chromium"
# Everything the driver needs is read back from the database the builder just
# wrote: the seed's own finding supplies the claim version the new finding
# must pin, and main's head state is the cut point the proposal must name (a
# non-empty base_ref is used VERBATIM as a state id, so a branch that named
# the wrong one would propose a sprawling diff against another branch).
CLAIM_VERSION_ID="$(q "select payload->'claim_version_refs'->>0 from scientific_object_versions where payload->'metadata'->>'seed_key'='find-f1' limit 1")"
MAIN_BASE_STATE_ID="$(q "select coalesce(base_state_id::text,'') from branches where id='$MAIN_BRANCH_ID'")"
# The provider repository the [git] step pushes to: real coordinates read from
# the platform's own provisioning row, and the same service token the gate
# already minted for cmd/api. No clone URL is pasted together anywhere else.
GIT_OWNER="$(q "select owner from git_repository_provisions where project_id='$PROJECT_ID'")"
GIT_REPO="$(q "select name from git_repository_provisions where project_id='$PROJECT_ID'")"
if [ -z "$CLAIM_VERSION_ID" ] || [ -z "$MAIN_BASE_STATE_ID" ] || [ -z "$GIT_OWNER" ] || [ -z "$GIT_REPO" ]; then
  fail "the run's inputs are missing: claim_version_id=${CLAIM_VERSION_ID:-none} main_head_state=${MAIN_BASE_STATE_ID:-none} provider_repo=${GIT_OWNER:-none}/${GIT_REPO:-none}"
else
  OWNER_EMAIL="$(python3 -c "import json;u=[u for u in json.load(open('$PLAN'))['users'] if u['key']=='owner'][0];print(u['email'])")"
  OWNER_PW="$(python3 -c "import json;u=[u for u in json.load(open('$PLAN'))['users'] if u['key']=='owner'][0];print(u['password'])")"
  EXT_EMAIL="$(python3 -c "import json;u=[u for u in json.load(open('$PLAN'))['users'] if u['key']=='external'][0];print(u['email'])")"
  EXT_PW="$(python3 -c "import json;u=[u for u in json.load(open('$PLAN'))['users'] if u['key']=='external'][0];print(u['password'])")"
  python3 - "$WORK/context.json" "$API_BASE" "$WEB_BASE" "$PROJECT_ID" "$MAIN_BRANCH_ID" \
    "$MAIN_BASE_STATE_ID" "$CLAIM_VERSION_ID" "$(date +%s)" "$OWNER_EMAIL" "$OWNER_PW" "$EXT_EMAIL" "$EXT_PW" \
    "$GITEA_BASE" "$GIT_OWNER" "$GIT_REPO" "$GITEA_TOKEN" <<'PY'
import json, sys
out, api, web, pid, main, head, claim, tag, oe, op, ee, ep, gbase, gowner, grepo, gtoken = sys.argv[1:17]
json.dump({
    "api_base": api, "web_base": web, "project_id": pid,
    "main_branch_id": main, "main_base_state_id": head,
    "claim_version_id": claim, "tag": tag,
    "owner": {"email": oe, "password": op},
    "external": {"email": ee, "password": ep},
    # The provider repository a [git] step pushes to. The token rides this
    # file (mode 600, in the run's own temp dir) because git needs it; it is
    # never printed: git is given it through GIT_CONFIG_VALUE_0, never in a
    # URL or an argument.
    "git": {"base": gbase, "owner": gowner, "repo": grepo, "token": gtoken},
}, open(out, "w"), indent=2)
PY
  chmod 600 "$WORK/context.json"
  # This step's own assertion is printed after the driver exits (its subject is
  # whether every step inside the driver passed, and that is not known until
  # then), so say where the driver's numbering takes over.
  printf '     the driver numbers its own steps from [%02d] up; this step''s own line follows them\n' "$((STEP + 1))"
  DRIVER_RC=0
  MOF_STEP_OFFSET="$STEP" node "$DRIVER" "$WEB_BASE" "$WORK/context.json" "$WORK/browser-result.json" \
    > "$WORK/browser.log" 2>&1 || DRIVER_RC=$?
  # The driver's own steps are printed verbatim: a red hop has to be readable
  # here, with the step it belongs to and what it actually saw.
  sed -n '1,400p' "$WORK/browser.log"
  if [ "$DRIVER_RC" = 0 ]; then ok "every browser step passed: sign-in, the proposal, both judgments through the app's own form, the non-member refusal, the merge"
  else fail "the browser driver exited $DRIVER_RC — the FAIL lines above name the hop, what was expected and what arrived"; fi
  # The driver numbers its steps from this gate's counter; continue from the
  # last one it reached, so no two steps in this log share a number (and a
  # red step can be pointed at by number alone).
  DRIVER_LAST="$(python3 -c "import json;print(json.load(open('$WORK/browser-result.json')).get('last_step',0))" 2>/dev/null)"
  if [ -n "$DRIVER_LAST" ] && [ "$DRIVER_LAST" -gt "$STEP" ] 2>/dev/null; then STEP="$DRIVER_LAST"; fi
fi

# --- what the merge actually did, measured outside the browser -------------
step "post: main gained exactly one state commit — acceptance is a state transition, not a flag"
MAIN_STATE_AFTER="$(q "select count(*) from state_commits where branch_id='$MAIN_BRANCH_ID'")"
if [ -n "$MAIN_STATE_AFTER" ]; then
  assert_eq "state commits on main (before → after)" "$((MAIN_STATE_BEFORE + 1))" "$MAIN_STATE_AFTER"
else fail "could not read main's state commits after the merge"; fi

step "post: the proposal is recorded as merged, and main's head IS the state the merge named"
PR_NUMBER="$(python3 -c "import json;print(json.load(open('$WORK/browser-result.json')).get('pr_number',''))" 2>/dev/null)"
MERGED_STATE="$(python3 -c "import json;print((json.load(open('$WORK/browser-result.json')).get('merge') or {}).get('state_id',''))" 2>/dev/null)"
GIT_SHA="$(python3 -c "import json;print((json.load(open('$WORK/browser-result.json')).get('merge') or {}).get('git_sha',''))" 2>/dev/null)"
if [ -n "$MERGED_STATE" ] && [ -n "$PR_NUMBER" ]; then
  assert_eq "the proposal's stored state and main's head (state the merge returned: $MERGED_STATE, provider commit: $GIT_SHA)" \
    "merged|$MERGED_STATE" \
    "$(q "select state from pull_requests where project_id='$PROJECT_ID' and number='$PR_NUMBER'")|$(q "select coalesce(base_state_id::text,'') from branches where id='$MAIN_BRANCH_ID'")"
else fail "the browser reported no merged state (pr=${PR_NUMBER:-none}) — the merge itself did not happen, so there is nothing to check here"; fi

step "post: the provider's OWN main ref carries exactly the commit the merge named"
# The Git truth, read from Gitea itself rather than from the merge's answer:
# the merge says which sha it advanced main to, and this asks the provider what
# its ref actually holds. A merge that reported a sha Gitea never received —
# or one that only moved the database — fails here.
if [ -n "$GIT_SHA" ] && [ -n "$GIT_OWNER" ]; then
  # The credential rides the environment (GIT_CONFIG_VALUE_0), never argv:
  # a token in a command line is readable by every account on the machine.
  LS_REMOTE_MAIN="$(env \
    GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader \
    GIT_CONFIG_VALUE_0="Authorization: token $GITEA_TOKEN" \
    git ls-remote "$GITEA_BASE/$GIT_OWNER/$GIT_REPO.git" refs/heads/main 2>"$WORK/ls-remote.err" \
    | awk '{print $1}')"
  if [ -n "$LS_REMOTE_MAIN" ]; then
    assert_eq "the sha Gitea's refs/heads/main holds (the merge named ${GIT_SHA:0:12}…)" "$GIT_SHA" "$LS_REMOTE_MAIN"
  else
    fail "git ls-remote could not read refs/heads/main from $GIT_OWNER/$GIT_REPO at $GITEA_BASE: $(head -1 "$WORK/ls-remote.err" 2>/dev/null)"
  fi
else
  fail "there is no provider commit to look for: the merge reported git_sha=${GIT_SHA:-none} and the provider repo is ${GIT_OWNER:-none}/${GIT_REPO:-none}"
fi

step "post: the accepted object has a version in main, in the very state the merge wrote"
GATE_OBJECT_ID="$(python3 -c "import json;print(json.load(open('$WORK/browser-result.json')).get('object_id',''))" 2>/dev/null)"
if [ -n "$GATE_OBJECT_ID" ] && [ -n "$MERGED_STATE" ]; then
  MAIN_OBJECT_STATE="$(q "select coalesce(v.state_id::text,'') from scientific_object_versions v where v.object_id='$GATE_OBJECT_ID' and v.branch_id='$MAIN_BRANCH_ID' limit 1")"
  assert_eq "the object the gate proposed, read from main" "$MERGED_STATE" "$MAIN_OBJECT_STATE"
else fail "the browser reported no object id (the proposal's object was never created)"; fi

# --- the end of the chain --------------------------------------------------
# Through here the assertions do their own HTTP, so a failed request and a
# false assertion cannot be reported as two failures of one step.
step "terminal: the external group's evidence is retrievable by the persistent id it was published under"
H1_PID="$(q "select kp.pid from knowledge_publications kp join scientific_object_versions v on v.id=kp.object_version_id where v.payload->'metadata'->>'seed_key'='hyp-h1' limit 1")"
DETAIL="$(python3 - "$API_BASE" "$H1_PID" "$FORK_ID" <<'PY' 2>&1
import json, sys, urllib.request
api, pid, fork = sys.argv[1:4]
if not pid:
    print("no publication pid for hyp-h1 — the seed never published the hypothesis")
    raise SystemExit(1)
try:
    with urllib.request.urlopen(f"{api}/api/v1/knowledge/{pid}", timeout=30) as r:
        doc = json.loads(r.read().decode())
except Exception as exc:  # noqa: BLE001 — the message is the evidence
    print(f"GET /api/v1/knowledge/{pid} failed: {exc}")
    raise SystemExit(1)
# The read document renders the evidence section at the TOP level of the
# document (cmd/api/knowledgehttp/read.go: the read payload carries
# `evidence` beside `payload`, not inside it).
ev = (doc.get("evidence") or (doc.get("payload") or {}).get("evidence")) or {}
ext = list(ev.get("unreviewed_external") or []) + list(ev.get("reviewed_external") or [])
mine = [a for a in ext if a.get("asserting_project_id") == fork]
if not mine:
    print(f"the document carries {len(ev.get('origin') or [])} origin assertion(s) but none from the fork {fork};"
          f" unreviewed_external={len(ev.get('unreviewed_external') or [])}"
          f" reviewed_external={len(ev.get('reviewed_external') or [])}")
    raise SystemExit(1)
print(f"pid {pid} renders {len(mine)} assertion(s) from the external group: "
      + ", ".join(f"{a.get('relation')}/{a.get('review_state')}" for a in mine[:4]))
PY
)"
if [ $? -eq 0 ]; then ok "$DETAIL"; else fail "docs/31 Gate I's external-evidence terminal is not reachable by pid — $DETAIL"; fi

step "terminal: contribution credit lands on the external contributor's own Research Profile"
DETAIL="$(python3 - "$API_BASE" "$EXTERNAL_USER_ID" <<'PY' 2>&1
import json, sys, urllib.request
api, uid = sys.argv[1:3]
try:
    with urllib.request.urlopen(f"{api}/api/v1/users/{uid}/research-profile", timeout=30) as r:
        p = json.loads(r.read().decode())
except Exception as exc:  # noqa: BLE001
    print(f"GET /api/v1/users/{uid}/research-profile failed: {exc}")
    raise SystemExit(1)
person = p.get("person") or {}
if person.get("id") != uid:
    print(f"the profile answered for {person.get('id')}, not for {uid}")
    raise SystemExit(1)
rows = p.get("contributions") or []
if not rows:
    print(f"{person.get('handle')} carries no contribution row at all"
          f" (affiliations={len(p.get('affiliations') or [])} assets={len(p.get('assets') or [])}"
          f" reproductions={len(p.get('reproductions') or [])})")
    raise SystemExit(1)
kinds = sorted({row.get("event_type") for row in rows})
print(f"{person.get('handle')} is credited with {len(rows)} contribution row(s): {', '.join(kinds)}")
PY
)"
if [ $? -eq 0 ]; then ok "$DETAIL"; else fail "the chain's last hop — external work credited to the contributor — did not land — $DETAIL"; fi

step "terminal: the organization affiliation does not swallow the personal history (docs/31 Gate D)"
DETAIL="$(python3 - "$API_BASE" "$EXTERNAL_USER_ID" <<'PY' 2>&1
import json, sys, urllib.request
api, uid = sys.argv[1:3]
try:
    with urllib.request.urlopen(f"{api}/api/v1/users/{uid}/research-profile", timeout=30) as r:
        p = json.loads(r.read().decode())
except Exception as exc:  # noqa: BLE001
    print(f"GET /api/v1/users/{uid}/research-profile failed: {exc}")
    raise SystemExit(1)
aff, con = p.get("affiliations") or [], p.get("contributions") or []
if not aff or not con:
    print(f"affiliations={len(aff)} contributions={len(con)} — both have to be present: the"
          " affiliation is a context ON the work, never a replacement for the person's own record")
    raise SystemExit(1)
names = ", ".join((a.get("organization") or {}).get("slug", "?") for a in aff[:3])
print(f"affiliations={len(aff)} ({names}) and contributions={len(con)} are rendered side by side")
PY
)"
if [ $? -eq 0 ]; then ok "$DETAIL"; else fail "the personal history and the organization affiliation are not both rendered — $DETAIL"; fi

# --- the negative coverage -------------------------------------------------
step "NEGATIVE: the version the demo published from the private branch IS discoverable"
PUB_STATUS="$(curl -sS -o "$WORK/feed-pub.xml" -w '%{http_code}' --max-time 30 "$API_BASE/api/v1/feeds/knowledge/$PRIV_PUBLISHED_OBJ" 2>/dev/null)"
PUB_ENTRIES="$(grep -o '<entry>' "$WORK/feed-pub.xml" 2>/dev/null | wc -l | tr -d ' ')"
assert_eq "HTTP status and entries in its public knowledge feed" "200|1" "${PUB_STATUS:-none}|${PUB_ENTRIES}"

step "NEGATIVE: its unpublished sibling is NOT — a private branch is not published wholesale"
UNPUB_STATUS="$(curl -sS -o "$WORK/feed-unpub.xml" -w '%{http_code}' --max-time 30 "$API_BASE/api/v1/feeds/knowledge/$PRIV_UNPUBLISHED_OBJ" 2>/dev/null)"
UNPUB_ENTRIES="$(grep -o '<entry>' "$WORK/feed-unpub.xml" 2>/dev/null | wc -l | tr -d ' ')"
assert_eq "publications the knowledge network names for the unpublished version (HTTP ${UNPUB_STATUS:-none})" "0" "$UNPUB_ENTRIES"

step "the worktree is still the one this gate started from"
assert_eq "git status --porcelain (a gate that changes the tree it grades has graded nothing)" "$DIRT_BEFORE" "$(git status --porcelain)"

summarise
