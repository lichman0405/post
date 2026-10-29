#!/usr/bin/env bash
# WEB-01 real-UI acceptance harness (docs/70 §6, G23).
#
# Starts an ISOLATED real stack and drives it with a real Chromium:
#
#   PostgreSQL   a fresh database per run (webv2_t1302_<run id>), migrated
#                and dropped by the Go harness;
#   Redis        the real auth session store and rate limiter, using a
#                dedicated DB index; this run's session tokens are revoked
#                on shutdown (never a FLUSH of anybody else's keys);
#   Go API       the production authhttp surface, guard and CORS allow-list
#                composed exactly as cmd/api composes them;
#   Next web     a production build served by `next start`.
#
# Then it runs, in order:
#
#   1. the static anti-bypass scan over every journey/helper module;
#   2. the entry-point pre-flight (sign-in form + account-establishment
#      entry) — missing entry fails, it never skips;
#   3. the browser journey: sign up through the DOM, the signed-in home's
#      project entry works, the session survives a reload and a second page,
#      sign out, a wrong password is reported recoverably, signing in from
#      another page returns to it, the session survives another reload, and
#      a session ended elsewhere offers a recoverable sign-in prompt;
#   4. the WEB-03 (U02) journey: organizations, invites, roles, leave;
#   5. the WEB-04 journey (T1305): a new account's empty directory offers the
#      first research path, a project is created through D16's Public default
#      and a second one through an explicit Private choice, a duplicate
#      address is a field error that creates nothing, and a fresh anonymous
#      context can see neither the private project nor its existence.
#      NOTE the order: this journey CREATES projects, so it runs after the
#      ones whose assertions are about a new account's empty directory.
#   6. the WEB-05 journey (T1306): the Research tab of a seeded PRIVATE
#      project — the map and the list agree about the object set in both
#      views, a question, an experiment and a finding each reach their old
#      version by clicking, and the return path keeps the object and its
#      version. Its research state is seeded (harness/fixture.go) because no
#      web surface can author one yet; the journey only reads it and claims
#      no creation step (docs/70 §6.3). The fixture owner signs in through
#      the product's /login form with credentials both sides derive from
#      WEB_V2_RUN_ID — never a constant, never recorded.
#   7. the WEB-06a journey (T1307): a fresh account creates its project and
#      two research branches through the DOM before any write, saves a
#      minimal question draft, signs out and back in and continues the draft
#      from the branch URL it saved, authors a hypothesis, an experiment
#      (with a RECORDED FAILURE) and a calculation through their own forms,
#      runs the server's own pr check (which names the incomplete
#      hypothesis's gap and says nothing was submitted), adds a relation and
#      a second version, then switches branch: the other branch holds only
#      its own complete object and its check passes, and switching back
#      shows branch A's objects, versions and relation intact. Unlike the
#      WEB-05 journey it seeds nothing: every object in it is typed.
#
# No business step uses page.request/APIRequestContext, SQL, CLI or a seeded
# account. Accounts are established by the browser through /login.
#
# Usage: bash tests/web-v2/run.sh [--preflight-only] [--only=<journey>,...]
#        --only is a DEVELOPMENT filter for iterating on one journey: it
#        still runs the dependency checks, the anti-bypass scan and the
#        pre-flight, and it records the filter in runner.json. A filtered run
#        is not a full acceptance run and does not claim to be one.
# Env:   WEB_V2_WEB_PORT           web port (default 31119)
#        WEB_V2_EVIDENCE_DIR       evidence output (default tests/web-v2/evidence/<run id>)
#        WEB_V2_RUN_ID             run id (default timestamp + random)
#        WEB_V2_POSTGRES_ADMIN_URL / POSTGRES_TEST_ADMIN_URL
#                                  (default: local compose DSN, set below;
#                                   the Go harness requires one to be set)
#        WEB_V2_REDIS_ADDR         default POST_REDIS_ADDR or 127.0.0.1:6379
#        WEB_V2_REDIS_DB           default 15 (isolated session namespace)
#        WEB_V2_API_ADDR           default 127.0.0.1:0 (kernel-assigned port)
#        WEB_V2_WEB_DIR            override the web app directory (tests)
#
# An unreachable service, a missing dependency, a busy port, a missing entry
# point or an unexecuted step is a FAILURE with the reason printed. There is
# no skip branch in this script.
set -euo pipefail

V2_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$V2_DIR/../.." && pwd)"
WEB_DIR="${WEB_V2_WEB_DIR:-$ROOT/apps/web}"
WEB_PORT="${WEB_V2_WEB_PORT:-31119}"
BASE="http://127.0.0.1:$WEB_PORT"
MODE="${1:-}"

RUN_ID="${WEB_V2_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')}"
EVIDENCE_DIR="${WEB_V2_EVIDENCE_DIR:-$V2_DIR/evidence/$RUN_ID}"
COMMIT="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
WORK_DIR="$(mktemp -d)"

export WEB_V2_RUN_ID="$RUN_ID"
export WEB_V2_COMMIT="$COMMIT"
export WEB_V2_EVIDENCE_DIR="$EVIDENCE_DIR"
export WEB_V2_WEB_ORIGIN="$BASE"
# Local dev default only: the documented compose value lives in this shell
# runner, never in the Go harness (which has no credential-bearing default
# and refuses to start without a named DSN). CI and any non-dev run set the
# variable explicitly.
export WEB_V2_POSTGRES_ADMIN_URL="${WEB_V2_POSTGRES_ADMIN_URL:-${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}}"
export WEB_V2_REDIS_ADDR="${WEB_V2_REDIS_ADDR:-${POST_REDIS_ADDR:-127.0.0.1:6379}}"
export WEB_V2_REDIS_DB="${WEB_V2_REDIS_DB:-15}"

WEB_PID=""
HARNESS_PID=""

cleanup() {
  if [ -n "$WEB_PID" ]; then
    pkill -P "$WEB_PID" 2>/dev/null || true
    kill "$WEB_PID" 2>/dev/null || true
    wait "$WEB_PID" 2>/dev/null || true
  fi
  if [ -n "$HARNESS_PID" ]; then
    kill "$HARNESS_PID" 2>/dev/null || true
    # The harness drops its database and revokes this run's sessions on
    # SIGTERM; give that shutdown a bounded moment rather than orphaning it.
    for _ in $(seq 1 50); do
      kill -0 "$HARNESS_PID" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$HARNESS_PID" 2>/dev/null || true
    wait "$HARNESS_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT INT TERM

fail() {
  echo "web-v2: FAILED — $1" >&2
  exit 1
}

fail_with_log() {
  echo "web-v2: FAILED — $1" >&2
  if [ -n "${2:-}" ] && [ -s "$2" ]; then
    echo "web-v2: last 60 lines of $2:" >&2
    tail -n 60 "$2" >&2
  fi
  exit 1
}

# ---- 1. Tools ---------------------------------------------------------------
for tool in node go pnpm curl; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing dependency: '$tool' is not on PATH; a skip is not a pass"
done

# ---- 2. Real services are reachable (harness preflight) ---------------------
echo "== web-v2: preflight (PostgreSQL + Redis + origin) =="
(
  cd "$ROOT"
  go build -o "$WORK_DIR/web-v2-harness" ./tests/web-v2/harness
) || fail "the harness did not compile"
if ! "$WORK_DIR/web-v2-harness" --preflight; then
  fail "service preflight failed: the run needs a reachable PostgreSQL and Redis (never skipped)"
fi

# ---- 3. Dependencies are present -------------------------------------------
if [ ! -d "$WEB_DIR/node_modules" ]; then
  fail "missing dependency: $WEB_DIR/node_modules; run 'pnpm install --frozen-lockfile' at the repository root (a skip is not a pass)"
fi
if [ ! -x "$WEB_DIR/node_modules/.bin/next" ]; then
  fail "missing dependency: $WEB_DIR/node_modules/.bin/next is absent; the production build cannot run"
fi
if [ ! -d "$V2_DIR/node_modules/playwright" ]; then
  if [ "$MODE" = "--preflight-only" ]; then
    fail "missing dependency: $V2_DIR/node_modules/playwright; run 'npm ci' in tests/web-v2"
  fi
fi

if [ "$MODE" = "--preflight-only" ]; then
  echo "web-v2: PREFLIGHT OK — real services reachable, web and harness dependencies present"
  exit 0
fi
ONLY=""
case "$MODE" in
  "") ;;
  --only=*) ONLY="${MODE#--only=}" ;;
  *) fail "unknown mode '$MODE' (accepted: --preflight-only, --only=<journey>,...)" ;;
esac

# Whether a journey section should run in this invocation. A --only list is a
# development filter; with no list every section runs.
wanted() {
  [ -z "$ONLY" ] && return 0
  case ",$ONLY," in
    *",$1,"*) return 0 ;;
    *) echo "-- skipping $1 (--only=$ONLY)"; return 1 ;;
  esac
}

# ---- 4. Port must be ours ---------------------------------------------------
if ss -tln 2>/dev/null | grep -qE ":$WEB_PORT[[:space:]]"; then
  fail "port $WEB_PORT is already in use; a previous run may have leaked a server, and reusing it would test someone else's build"
fi

mkdir -p "$EVIDENCE_DIR"

# ---- 5. Harness deps + browser ---------------------------------------------
echo "== web-v2: installing harness deps (own lockfile) and Chromium =="
(
  cd "$V2_DIR"
  npm ci --no-audit --no-fund
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
) || fail "harness dependency installation failed"
PLAYWRIGHT_VERSION="$(cd "$V2_DIR" && node -p 'require("playwright/package.json").version')"
export WEB_V2_PLAYWRIGHT_VERSION="$PLAYWRIGHT_VERSION"

LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")" || fail "browser library bootstrap failed"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

# ---- 6. Start the isolated real API ----------------------------------------
echo "== web-v2: starting the isolated real API (PostgreSQL + Redis) =="
"$WORK_DIR/web-v2-harness" > "$WORK_DIR/harness.log" 2>&1 &
HARNESS_PID=$!
READY_JSON="$WORK_DIR/ready.json"
ready=0
for _ in $(seq 1 120); do
  if grep -q '^READY ' "$WORK_DIR/harness.log" 2>/dev/null; then
    grep -m1 '^READY ' "$WORK_DIR/harness.log" | sed 's/^READY //' > "$READY_JSON"
    ready=1
    break
  fi
  if ! kill -0 "$HARNESS_PID" 2>/dev/null; then
    cp "$WORK_DIR/harness.log" "$EVIDENCE_DIR/harness.log" 2>/dev/null || true
    fail_with_log "the API harness exited before it was ready" "$WORK_DIR/harness.log"
  fi
  sleep 0.5
done
[ "$ready" = 1 ] || fail_with_log "the API harness did not become ready" "$WORK_DIR/harness.log"

API_URL="$(node -e 'const fs=require("fs"); process.stdout.write(JSON.parse(fs.readFileSync(process.argv[1],"utf8")).api_origin)' "$READY_JSON")"
export WEB_V2_API_ORIGIN="$API_URL"
export WEB_V2_HARNESS_JSON="$(cat "$READY_JSON")"
cp "$WORK_DIR/harness.log" "$EVIDENCE_DIR/harness.log"
echo "== web-v2: API ready at $API_URL (isolated database) =="

# ---- 7. Build and start the real web app -----------------------------------
echo "== web-v2: building @post/web against the isolated API =="
(
  cd "$WEB_DIR"
  POST_ENV=prod API_BASE_URL="$API_URL" SCIENTIFIC_ADAPTER_URL="http://127.0.0.1:19100" pnpm run build
) || fail "the web app production build failed"

echo "== web-v2: starting next on $BASE =="
(
  cd "$WEB_DIR"
  POST_ENV=prod API_BASE_URL="$API_URL" SCIENTIFIC_ADAPTER_URL="http://127.0.0.1:19100" \
    "$WEB_DIR/node_modules/.bin/next" start -p "$WEB_PORT" -H 127.0.0.1
) > "$WORK_DIR/web.log" 2>&1 &
WEB_PID=$!

web_ready=0
for _ in $(seq 1 120); do
  if curl -sf -o /dev/null "$BASE/"; then
    web_ready=1
    break
  fi
  if ! kill -0 "$WEB_PID" 2>/dev/null; then
    break
  fi
  sleep 0.5
done
if [ "$web_ready" != 1 ]; then
  cp "$WORK_DIR/web.log" "$EVIDENCE_DIR/web.log" 2>/dev/null || true
  fail_with_log "the web app did not become ready" "$WORK_DIR/web.log"
fi
echo "== web-v2: web ready =="

# ---- 8. Anti-bypass scan (journey + every helper) --------------------------
echo "== web-v2: static anti-bypass scan =="
node "$ROOT/scripts/tests/web-v2-bypass-scan.mjs" "$V2_DIR" || fail "the anti-bypass scan found a forbidden bypass in the journey or its helpers"

# ---- 9. Entry-point pre-flight ---------------------------------------------
echo "== web-v2: entry-point check (sign-in form + account establishment) =="
node "$V2_DIR/entrypoint-check.mjs" "$BASE/login" || fail "the login page is missing a required entry point (missing entry is a failure, never a skip)"

# ---- 9b. Authoring model unit tests (WEB-06a) ------------------------------
# The browser journey can only type the values it happens to type; these pin
# the value→payload conversion itself (an empty draft field must stay absent,
# an outcome must land in `metadata`, a gate warning must not count as a
# block) before the slower journeys run. They import the app's own module.
echo "== web-v2: authoring model unit tests (WEB-06a) =="
node --test "$V2_DIR/authoring.test.mjs" || fail "the research authoring model unit tests failed"

# ---- 10. Browser journey ----------------------------------------------------
if wanted base; then
  echo "== web-v2: browser journey (signup/session/signout/login) =="
  if ! node "$V2_DIR/journey.mjs" "$BASE"; then
    cp "$WORK_DIR/web.log" "$EVIDENCE_DIR/web.log" 2>/dev/null || true
    fail "the browser journey failed (evidence: $EVIDENCE_DIR)"
  fi
fi

# ---- 11. WEB-03 browser journey (organizations, members, roles) ------------
# Its own evidence subdirectory: two journeys write two step records, two
# traces and two screenshot series, and merging them into one run-id would
# make neither reviewable.
U02_EVIDENCE_DIR="$EVIDENCE_DIR/u02"
if wanted u02; then
  echo "== web-v2: browser journey U02 (organizations/members/roles) =="
  mkdir -p "$U02_EVIDENCE_DIR"
  if ! WEB_V2_EVIDENCE_DIR="$U02_EVIDENCE_DIR" node "$V2_DIR/journey-u02.mjs" "$BASE"; then
    cp "$WORK_DIR/web.log" "$EVIDENCE_DIR/web.log" 2>/dev/null || true
    fail "the WEB-03 (U02) browser journey failed (evidence: $U02_EVIDENCE_DIR)"
  fi
fi

# ---- 12. WEB-04 browser journey (create a project, and who may see it) ------
# Runs after the journeys that assert a new account's EMPTY directory, on
# purpose: it creates projects. The journey writes its own evidence under
# $EVIDENCE_DIR/web04 (it appends "web04"), so it cannot overwrite another
# journey's metadata/steps/trace.
if wanted web04; then
  echo "== web-v2: browser journey (WEB-04 create: Public default, Private choice, duplicate address, anonymous hiding) =="
  if ! node "$V2_DIR/journey-web04.mjs" "$BASE"; then
    cp "$WORK_DIR/web.log" "$EVIDENCE_DIR/web.log" 2>/dev/null || true
    fail "the WEB-04 browser journey failed (evidence: $EVIDENCE_DIR/web04)"
  fi
fi

# ---- 13. WEB-05 browser journey (research objects, versions, return path) ---
# Reads the PRIVATE browse fixture the harness seeded (harness/fixture.go) as
# its owner, signed in through /login. It creates no state, so its position
# relative to the journeys above is free. Its own evidence goes under
# $EVIDENCE_DIR/web05.
if wanted web05; then
  echo "== web-v2: browser journey (WEB-05 browse: map/list agreement, old versions, return path) =="
  if ! node "$V2_DIR/journey-web05.mjs" "$BASE"; then
    cp "$WORK_DIR/web.log" "$EVIDENCE_DIR/web.log" 2>/dev/null || true
    fail "the WEB-05 browser journey failed (evidence: $EVIDENCE_DIR/web05)"
  fi
fi

# ---- 14. WEB-06a browser journey (branches, drafts, the four forms) --------
# Creates its own account and project and every object it asserts; it seeds
# nothing, so it may run anywhere. Its own evidence goes under
# $EVIDENCE_DIR/web06.
WEB06_EVIDENCE_DIR="$EVIDENCE_DIR/web06"
if wanted web06; then
  echo "== web-v2: browser journey (WEB-06a author: branches, draft continuation, four forms, review check, relation) =="
  mkdir -p "$WEB06_EVIDENCE_DIR"
  if ! WEB_V2_EVIDENCE_DIR="$WEB06_EVIDENCE_DIR" node "$V2_DIR/journey-web06.mjs" "$BASE"; then
    cp "$WORK_DIR/web.log" "$EVIDENCE_DIR/web.log" 2>/dev/null || true
    fail "the WEB-06a authoring journey failed (evidence: $WEB06_EVIDENCE_DIR)"
  fi
fi

cp "$WORK_DIR/web.log" "$EVIDENCE_DIR/web.log" 2>/dev/null || true

# A filtered run is NOT an acceptance run: --only skips journeys, so "every
# selected journey passed" is all it can say. runner.json and the banner say
# "partial" so a filtered run can never be quoted as the full web-v2 gate.
if [ -n "$ONLY" ]; then
  RESULT="partial"
else
  RESULT="passed"
fi
cat > "$EVIDENCE_DIR/runner.json" <<EOF
{
  "runner": "tests/web-v2/run.sh",
  "run_id": "$RUN_ID",
  "commit": "$COMMIT",
  "web_origin": "$BASE",
  "api_origin": "$API_URL",
  "result": "$RESULT",
  "evidence_dir": "$EVIDENCE_DIR",
  "u02_evidence_dir": "$U02_EVIDENCE_DIR",
  "web05_evidence_dir": "$EVIDENCE_DIR/web05",
  "web06_evidence_dir": "$WEB06_EVIDENCE_DIR",
  "only": "$ONLY"
}
EOF
if [ "$RESULT" = "partial" ]; then
  echo "web-v2: PARTIAL — only the $ONLY journey(s) ran; this is a development filter, not an acceptance run (evidence: $EVIDENCE_DIR)"
  exit 0
fi
echo "web-v2: PASSED — evidence in $EVIDENCE_DIR"
