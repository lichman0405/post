#!/usr/bin/env bash
# Explore surface browser e2e (T0802): builds the web app, serves it with
# `next start`, brings up the harness's API origin, and drives /explore in
# real Chromium.
#
# One difference from tests/e2e-assets worth stating up front: /explore is a
# SERVER component (page.tsx reads the index in the Next server process), so
# Playwright cannot intercept its fetch — the mock has to be a real origin on
# a real port, which is what mock-api.mjs is. See that file's header for what
# the mock does and does not stand in for: the wire shape is locked against
# the real handler by tests/e2e/explore_e2e_test.go and against real
# PostgreSQL by tests/integration/explore_test.go; this harness decides
# whether the PAGE renders and drives that index.
#
# Self-contained like tests/e2e-files and tests/e2e-assets: the harness deps
# live in this directory's own npm project (its own lockfile), outside the
# pnpm workspace, so the repository's pnpm-lock.yaml is never touched.
#
# Usage: bash tests/e2e-explore/run.sh
# Env:   EXPLORE_E2E_PORT (default 31122)        the web app
#        EXPLORE_E2E_API_PORT (default 31123)    the harness's API origin
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$E2E_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
PORT="${EXPLORE_E2E_PORT:-31122}"
API_PORT="${EXPLORE_E2E_API_PORT:-31123}"
BASE="http://127.0.0.1:$PORT"
API="http://127.0.0.1:$API_PORT"

# The API origin is baked into the web app's configuration; here it is the
# harness's own origin, reachable over loopback from the Next server
# process. Nothing in this run leaves the machine.
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
    echo "e2e-explore: port $p is already in use; a previous run may have leaked a server" >&2
    exit 1
  fi
done

echo "== e2e-explore: installing harness deps (own lockfile, npm) =="
(
  cd "$E2E_DIR"
  npm install --no-audit --no-fund
  # Chromium headless shell only. The browsers are already in the shared
  # Playwright cache; this call is a no-op when they are.
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

echo "== e2e-explore: browser library bootstrap (shared web-smoke helper) =="
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

echo "== e2e-explore: building the web app (API origin baked in) =="
(
  cd "$WEB_DIR"
  pnpm run build
)

echo "== e2e-explore: starting the harness API origin on $API =="
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
  echo "e2e-explore: the API origin did not become ready; log:" >&2
  cat "$E2E_DIR/mock.log" >&2
  exit 1
fi

echo "== e2e-explore: starting next on $BASE =="
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
  echo "e2e-explore: server did not become ready; log:" >&2
  cat "$E2E_DIR/server.log" >&2
  exit 1
fi
echo "== e2e-explore: servers ready =="

echo "== e2e-explore: index checklist =="
if ! node "$E2E_DIR/explore-e2e.mjs" "$BASE" "$API"; then
  echo "e2e-explore: FAILED (server log: $E2E_DIR/server.log)" >&2
  exit 1
fi
echo "e2e-explore: all passed"
