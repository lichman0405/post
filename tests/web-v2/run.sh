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
#      a session ended elsewhere offers a recoverable sign-in prompt.
#
# No business step uses page.request/APIRequestContext, SQL, CLI or a seeded
# account. Accounts are established by the browser through /login.
#
# Usage: bash tests/web-v2/run.sh [--preflight-only]
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
if [ -n "$MODE" ]; then
  fail "unknown mode '$MODE' (only --preflight-only is accepted)"
fi

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

# ---- 10. Browser journey ----------------------------------------------------
echo "== web-v2: browser journey (signup/session/signout/login) =="
if ! node "$V2_DIR/journey.mjs" "$BASE"; then
  cp "$WORK_DIR/web.log" "$EVIDENCE_DIR/web.log" 2>/dev/null || true
  fail "the browser journey failed (evidence: $EVIDENCE_DIR)"
fi

cp "$WORK_DIR/web.log" "$EVIDENCE_DIR/web.log" 2>/dev/null || true
cat > "$EVIDENCE_DIR/runner.json" <<EOF
{
  "runner": "tests/web-v2/run.sh",
  "run_id": "$RUN_ID",
  "commit": "$COMMIT",
  "web_origin": "$BASE",
  "api_origin": "$API_URL",
  "result": "passed",
  "evidence_dir": "$EVIDENCE_DIR"
}
EOF
echo "web-v2: PASSED — evidence in $EVIDENCE_DIR"
