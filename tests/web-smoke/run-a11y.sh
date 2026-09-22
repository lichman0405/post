#!/usr/bin/env bash
# Full a11y suite (T1104): scans every docs/42 core page that has a route,
# with real fixture data for the dynamic routes. Starts the a11y-harness
# (tests/web-smoke/a11y-harness), builds the web app against it, and drives
# a11y-smoke.mjs with the harness's READY ids.
#
# Usage: bash tests/web-smoke/run-a11y.sh
# Env:   A11Y_API_ADDR     (default 127.0.0.1:18193)
#        A11Y_WEB_PORT     (default 31108)
#        POSTGRES_TEST_ADMIN_URL
#
# Every phase prints its own wall-clock cost. CI has to budget for this step
# (it installs Chromium and builds Next), so the numbers are reported by the
# run rather than estimated in a comment that goes stale.
set -euo pipefail

SUITE_START=$SECONDS
phase() { printf 'a11y-suite: [%3ds] %s\n' "$((SECONDS - SUITE_START))" "$1"; }

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SMOKE_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
API_PORT="${A11Y_API_ADDR:-127.0.0.1:18193}"
WEB_PORT="${A11Y_WEB_PORT:-31108}"
API_URL="http://$API_PORT"
BASE="http://127.0.0.1:$WEB_PORT"

export POSTGRES_TEST_ADMIN_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
export A11Y_API_ADDR="$API_PORT"
export A11Y_WEB_ORIGIN="$BASE"

WORK_DIR="$(mktemp -d)"
WEB_PID=""
API_PID=""
# The logs live in $WORK_DIR, which the EXIT trap below deletes. A failure
# path that says only "...; log:" therefore handed the reader a colon and
# nothing else — the file it named was gone by the time they looked. Every
# "not ready" failure goes through this instead, so the diagnostics survive
# the cleanup that follows the exit.
fail_with_log() {
  echo "a11y-suite: $1" >&2
  if [ -s "$2" ]; then
    echo "a11y-suite: last 40 lines of $(basename "$2") ($WORK_DIR, removed on exit):" >&2
    tail -n 40 "$2" >&2
  else
    echo "a11y-suite: $2 is empty or missing" >&2
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
    echo "a11y-suite: port $port is already in use; a previous run may have leaked a server" && exit 1
  fi
done

phase "installing harness deps (own lockfile, npm) + Chromium"
(
  cd "$SMOKE_DIR"
  # npm ci, not npm install: this harness decides WCAG conformance, so the
  # versions it decides with (playwright 1.55.0, axe-core 4.10.3) come from
  # tests/web-smoke/package-lock.json and are not re-resolved from the
  # registry on every run.
  npm ci --no-audit --no-fund
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

phase "browser library bootstrap (shared helper)"
LIB_PATH="$(bash "$SMOKE_DIR/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

phase "building the API harness"
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

phase "axe-core + keyboard scan (real Chromium)"
node "$SMOKE_DIR/a11y-smoke.mjs" "$BASE" "$ready_json"
phase "SUITE PASSED (total)"
