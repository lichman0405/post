#!/usr/bin/env bash
# T0410 required test "playwright pr flows": the seed project's branch/PR
# journey (docs/34) driven in real Chromium against the REAL stack — the
# Go harness in tests/e2e-pr-flows/harness composes the production
# handlers, stores and guard over a real PostgreSQL and a real Gitea, the
# web app is the built app under `next start`, and the browser talks to the
# API itself (real CORS preflights, real session cookie, real CSRF token).
# Nothing is mocked: this is the one browser suite that needs the local
# dev stack (PostgreSQL, Gitea, and the git CLI on PATH) to be up.
#
# Usage: bash tests/e2e-pr-flows/run.sh
# Env:   PR_FLOWS_WEB_PORT (default 31160)   the web app's port
#        PR_FLOWS_API_PORT (default 18080)   the harness API's port
#        POSTGRES_TEST_ADMIN_URL             as the integration suite
#        POST_GITEA_BASE_URL                 default http://127.0.0.1:3000
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$E2E_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
WEB_PORT="${PR_FLOWS_WEB_PORT:-31160}"
API_PORT="${PR_FLOWS_API_PORT:-18080}"
BASE="http://127.0.0.1:$WEB_PORT"
API="http://127.0.0.1:$API_PORT"

export POSTGRES_TEST_ADMIN_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
export POST_GITEA_BASE_URL="${POST_GITEA_BASE_URL:-http://127.0.0.1:3000}"

# The web app validates its own environment at startup AND at build time
# (lib/config.ts fails closed): the API origin is baked into the client
# bundle by the build, and the same three variables are read again by the
# server process, so they are exported rather than passed to one command.
export POST_ENV=prod
export API_BASE_URL="$API"
export SCIENTIFIC_ADAPTER_URL="${SCIENTIFIC_ADAPTER_URL:-http://127.0.0.1:19100}"

WORK_DIR="$(mktemp -d)"
WEB_PID=""
API_PID=""
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

# Never test against a stale server: a previous run whose wrapper died would
# otherwise answer for this one's assertions.
for port in "$WEB_PORT" "$API_PORT"; do
  if ss -tln 2>/dev/null | grep -qE ":$port[[:space:]]"; then
    echo "e2e-pr-flows: port $port is already in use; a previous run may have leaked a server" >&2
    exit 1
  fi
done

echo "== e2e-pr-flows: installing harness deps (own lockfile, npm) =="
(
  cd "$E2E_DIR"
  npm install --no-audit --no-fund
  # Chromium headless shell only; a no-op when the shared cache has it.
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

echo "== e2e-pr-flows: browser library bootstrap (shared web-smoke helper) =="
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

echo "== e2e-pr-flows: building the API harness =="
(
  cd "$ROOT"
  go build -o "$WORK_DIR/pr-flows-harness" ./tests/e2e-pr-flows/harness
)

echo "== e2e-pr-flows: building the web app (API origin baked in) =="
(
  cd "$WEB_DIR"
  pnpm run build
)

echo "== e2e-pr-flows: starting the API harness on $API =="
"$WORK_DIR/pr-flows-harness" -addr "127.0.0.1:$API_PORT" -web-origin "$BASE" \
  > "$WORK_DIR/harness.log" 2>&1 &
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
    echo "e2e-pr-flows: the API harness exited before it was ready; log:" >&2
    cat "$WORK_DIR/harness.log" >&2
    exit 1
  fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "e2e-pr-flows: the API harness did not become ready; log:" >&2
  cat "$WORK_DIR/harness.log" >&2
  exit 1
fi
echo "== e2e-pr-flows: the API harness is ready (own database, real Gitea) =="

echo "== e2e-pr-flows: starting next on $BASE =="
(
  cd "$WEB_DIR"
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
  echo "e2e-pr-flows: the web app did not become ready; log:" >&2
  cat "$WORK_DIR/web.log" >&2
  exit 1
fi
echo "== e2e-pr-flows: web app ready =="

echo "== e2e-pr-flows: the two flows, in Chromium =="
if ! PR_FLOWS_SHOT_DIR="$WORK_DIR" node "$E2E_DIR/pr-flows-e2e.mjs" "$BASE" "$ready_json"; then
  echo "e2e-pr-flows: FAILED (harness log: $WORK_DIR/harness.log, web log: $WORK_DIR/web.log)" >&2
  tail -40 "$WORK_DIR/harness.log" >&2 || true
  exit 1
fi
echo "e2e-pr-flows: all passed"
