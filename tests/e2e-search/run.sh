#!/usr/bin/env bash
# Search Answer browser e2e (T0907): builds the web app, serves it with
# `next start`, brings up the harness's API origin, and drives /search in
# real Chromium.
#
# Self-contained like tests/e2e-explore and tests/e2e-assets: the harness
# deps live in this directory's own npm project (its own lockfile), outside
# the pnpm workspace, so the repository's pnpm-lock.yaml is never touched.
#
# The API origin is a real listener rather than a `page.route`, and the reason
# is in mock-api.mjs's header: a source's href is a second document the browser
# navigates to, and "there is a server at that address" is exactly what the
# click assertion has to be able to be wrong about.
#
# Usage: bash tests/e2e-search/run.sh
# Env:   SEARCH_E2E_PORT (default 31130)        the web app
#        SEARCH_E2E_API_PORT (default 31131)    the harness's API origin
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$E2E_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
PORT="${SEARCH_E2E_PORT:-31130}"
API_PORT="${SEARCH_E2E_API_PORT:-31131}"
BASE="http://127.0.0.1:$PORT"
API="http://127.0.0.1:$API_PORT"

# The API origin is baked into the web app's configuration; here it is the
# harness's own origin, reachable over loopback from the Next server process
# and from the browser. Nothing in this run leaves the machine.
export POST_ENV=prod
export API_BASE_URL="$API"
export SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100

SERVER_PID=""
API_PID=""
cleanup() {
  for pid in "$SERVER_PID" "$API_PID"; do
    if [ -n "$pid" ]; then
      # Kill the process and any children it may have spawned.
      pkill -P "$pid" 2>/dev/null || true
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
}
trap cleanup EXIT

# Never test against a stale server: if a port is already taken (e.g. a
# previous run's process outlived its wrapper), fail loudly instead of
# silently exercising someone else's build.
for p in "$PORT" "$API_PORT"; do
  if ss -tln 2>/dev/null | grep -qE ":$p[[:space:]]"; then
    echo "e2e-search: port $p is already in use; a previous run may have leaked a server" >&2
    exit 1
  fi
done

echo "== e2e-search: installing harness deps (own lockfile, npm) =="
(
  cd "$E2E_DIR"
  npm install --no-audit --no-fund
  # Chromium headless shell only. The browsers are already in the shared
  # Playwright cache; this call is a no-op when they are.
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

echo "== e2e-search: browser library bootstrap (shared web-smoke helper) =="
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

echo "== e2e-search: building the web app (API origin baked in) =="
(
  cd "$WEB_DIR"
  pnpm run build
)

echo "== e2e-search: starting the harness API origin on $API =="
node "$E2E_DIR/mock-api.mjs" "$API_PORT" > "$E2E_DIR/mock.log" 2>&1 &
API_PID=$!

api_ready=0
for _ in $(seq 1 40); do
  if curl -sf -o /dev/null "$API/__stats"; then
    api_ready=1
    break
  fi
  sleep 0.25
done
if [ "$api_ready" != 1 ]; then
  echo "e2e-search: the API origin did not become ready; log:" >&2
  cat "$E2E_DIR/mock.log" >&2
  exit 1
fi

echo "== e2e-search: starting next on $BASE =="
(
  cd "$WEB_DIR"
  # Invoke the next binary directly (not through npx): the backgrounded
  # process is then the server itself, so the trap above can reliably kill
  # it instead of orphaning the real listener behind an npx wrapper.
  "$WEB_DIR/node_modules/.bin/next" start -p "$PORT"
) > "$E2E_DIR/server.log" 2>&1 &
SERVER_PID=$!

ready=0
for _ in $(seq 1 60); do
  if curl -sf -o /dev/null "$BASE/"; then
    ready=1
    break
  fi
  sleep 0.5
done
if [ "$ready" != 1 ]; then
  echo "e2e-search: server did not become ready; log:" >&2
  cat "$E2E_DIR/server.log" >&2
  exit 1
fi
echo "== e2e-search: servers ready =="

echo "== e2e-search: the answer checklist =="
if ! node "$E2E_DIR/search-e2e.mjs" "$BASE" "$API"; then
  echo "e2e-search: FAILED (server log: $E2E_DIR/server.log)" >&2
  exit 1
fi
echo "e2e-search: all passed"
