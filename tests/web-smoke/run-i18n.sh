#!/usr/bin/env bash
# Full i18n suite (T1105): drives i18n-smoke.mjs against the SAME harness
# T1104's a11y suite uses — tests/web-smoke/a11y-harness — with real fixture
# data for the dynamic routes, so the project shell's own localized labels are
# checked against a page that really loaded rather than against a not-found
# state.
#
# This is deliberately a PARALLEL target, not a replacement: `make a11y` and
# `make i18n` each build the same harness on their own port pair and each runs
# one check file. Neither is a precondition of the other, so neither can be
# deleted while the other "still passes". The harness, the browser bootstrap
# and the READY-ids contract are shared — there is one browser harness in this
# repository, and this script is the second driver of it, not a second one.
#
# Usage: bash tests/web-smoke/run-i18n.sh
# Env:   I18N_API_ADDR       (default 127.0.0.1:18194)
#        I18N_WEB_PORT       (default 31109)
#        POSTGRES_TEST_ADMIN_URL
#
# Every phase prints its own wall-clock cost, for the same reason run-a11y.sh
# does: CI has to budget for this step (it installs Chromium and builds Next),
# and a number printed by the run does not go stale the way a comment does.
set -euo pipefail

SUITE_START=$SECONDS
phase() { printf 'i18n-suite: [%3ds] %s\n' "$((SECONDS - SUITE_START))" "$1"; }

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SMOKE_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
# Distinct ports from run-a11y.sh (18193/31108) on purpose: the two suites are
# independent targets, and running one while the other is up must not look
# like a leaked server.
API_PORT="${I18N_API_ADDR:-127.0.0.1:18194}"
WEB_PORT="${I18N_WEB_PORT:-31109}"
API_URL="http://$API_PORT"
BASE="http://127.0.0.1:$WEB_PORT"

export POSTGRES_TEST_ADMIN_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
export A11Y_API_ADDR="$API_PORT"
export A11Y_WEB_ORIGIN="$BASE"

WORK_DIR="$(mktemp -d)"
WEB_PID=""
API_PID=""
fail_with_log() {
  echo "i18n-suite: $1" >&2
  if [ -s "$2" ]; then
    echo "i18n-suite: last 40 lines of $(basename "$2") ($WORK_DIR, removed on exit):" >&2
    tail -n 40 "$2" >&2
  else
    echo "i18n-suite: $2 is empty or missing" >&2
  fi
  exit 1
}
cleanup() {
  if [ -n "$WEB_PID" ]; then
    pkill -P "$WEB_PID" 2>/dev/null || true
    kill "$WEB_PID" 2>/dev/null || true
    wait "$WEB_PID" 2>/dev/null || true
  fi
  if [ -n "$API_PID" ]; then
    kill "$API_PID" 2>/dev/null || true
    wait "$API_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

for port in "$WEB_PORT" "${API_PORT##*:}"; do
  if ss -tln 2>/dev/null | grep -qE ":$port[[:space:]]"; then
    echo "i18n-suite: port $port is already in use; a previous run may have leaked a server" && exit 1
  fi
done

phase "installing harness deps (own lockfile, npm) + Chromium"
(
  cd "$SMOKE_DIR"
  npm ci --no-audit --no-fund
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

phase "browser library bootstrap (shared helper)"
LIB_PATH="$(bash "$SMOKE_DIR/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

# The static half of the suite first, against a server that has no API at all.
# It is the mode `tests/web-smoke/run.sh` drives this same check file in, and
# running it here too means a change that only breaks the services-down path
# cannot hide behind the fixtures below.
phase "building the API harness (shared with the a11y suite)"
(
  cd "$ROOT"
  go build -o "$WORK_DIR/a11y-harness" ./tests/web-smoke/a11y-harness
)

phase "starting the API harness on $API_URL"
"$WORK_DIR/a11y-harness" > "$WORK_DIR/harness.log" 2>&1 &
API_PID=$!

ready_json="$WORK_DIR/ready.json"
ready=0
for _ in $(seq 1 180); do
  if grep -q '^READY ' "$WORK_DIR/harness.log" 2>/dev/null; then
    grep -m1 '^READY ' "$WORK_DIR/harness.log" | sed 's/^READY //' > "$ready_json"
    ready=1
    break
  fi
  if ! kill -0 "$API_PID" 2>/dev/null; then
    fail_with_log "the API harness exited before it was ready" "$WORK_DIR/harness.log"
  fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  fail_with_log "the API harness did not become ready" "$WORK_DIR/harness.log"
fi
phase "API harness ready (own database, real PostgreSQL)"

phase "building the web app (API origin baked in)"
(
  cd "$WEB_DIR"
  POST_ENV=prod API_BASE_URL="$API_URL" SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100 pnpm run build
)

phase "starting next on $BASE"
(
  cd "$WEB_DIR"
  POST_ENV=prod API_BASE_URL="$API_URL" SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100 \
    "$WEB_DIR/node_modules/.bin/next" start -p "$WEB_PORT"
) > "$WORK_DIR/web.log" 2>&1 &
WEB_PID=$!

web_ready=0
for _ in $(seq 1 60); do
  if curl -sf -o /dev/null "$BASE/"; then
    web_ready=1
    break
  fi
  sleep 0.5
done
if [ "$web_ready" != 1 ]; then
  fail_with_log "the web app did not become ready" "$WORK_DIR/web.log"
fi
phase "web app ready"

phase "locale switching + wire-identity checks (real Chromium, fixture data)"
node "$SMOKE_DIR/i18n-smoke.mjs" "$BASE" "$ready_json"
phase "SUITE PASSED (total)"
