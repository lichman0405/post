#!/usr/bin/env bash
# Anonymous pages e2e (T0801 required test "anonymous e2e"): builds the web
# app, serves it with `next start`, and reads the public entity pages twice
# — as a crawler (plain HTTP, no JavaScript, no cookie) and as a signed-out
# visitor in real Chromium — against mock-api.mjs, a REAL HTTP server that
# answers exactly what cmd/api answers an anonymous caller.
#
# Why a real server here and not this repo's usual browser-level intercept:
# half of what T0801 adds is read by the NEXT SERVER, not by the browser —
# the <head> of every generateMetadata, /robots.txt and /sitemap.xml. A
# page.route() intercept lives inside the browser and cannot see those
# requests at all, so the harness would silently assert against a metadata
# fetch that never happened. The mock therefore listens on its own port and
# API_BASE_URL points at it, for the server and for the browser alike.
#
# The wire shapes it serves are the ones tests/e2e/privacy_e2e_test.go and
# the integration suite lock against the real guard and real PostgreSQL.
#
# Self-contained like tests/e2e-assets: this directory's own npm project and
# lockfile, outside the pnpm workspace, so the repository's pnpm-lock.yaml
# is never touched.
#
# Usage: bash tests/e2e-anonymous/run.sh
# Env:   ANON_E2E_PORT      (default 31141) overrides the web server port,
#        ANON_E2E_MOCK_PORT (default 31142) overrides the mock API port.
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$E2E_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
PORT="${ANON_E2E_PORT:-31141}"
MOCK_PORT="${ANON_E2E_MOCK_PORT:-31142}"
BASE="http://127.0.0.1:$PORT"
API="http://127.0.0.1:$MOCK_PORT"

export POST_ENV=prod
export API_BASE_URL="$API"
export SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100

MOCK_PID=""
SERVER_PID=""
cleanup() {
  for pid in "$SERVER_PID" "$MOCK_PID"; do
    if [ -n "$pid" ]; then
      # Kill the process and any children it may have spawned.
      pkill -P "$pid" 2>/dev/null || true
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
}
trap cleanup EXIT

# Never test against a stale server: if either port is already taken (e.g. a
# previous run's process outlived its wrapper), fail loudly instead of
# silently exercising someone else's build — or worse, someone else's mock.
for port in "$PORT" "$MOCK_PORT"; do
  if ss -tln 2>/dev/null | grep -qE ":$port[[:space:]]"; then
    echo "e2e-anonymous: port $port is already in use; a previous run may have leaked a server" >&2
    exit 1
  fi
done
if [ "$PORT" = "$MOCK_PORT" ]; then
  echo "e2e-anonymous: ANON_E2E_PORT and ANON_E2E_MOCK_PORT must differ" >&2
  exit 1
fi

echo "== e2e-anonymous: installing harness deps (own lockfile, npm) =="
(
  cd "$E2E_DIR"
  npm install --no-audit --no-fund
  # Chromium headless shell only. The browsers are already in the shared
  # Playwright cache; this call is a no-op when they are.
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

echo "== e2e-anonymous: browser library bootstrap (shared web-smoke helper) =="
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

echo "== e2e-anonymous: starting the mock API on $API =="
node "$E2E_DIR/mock-api.mjs" "$MOCK_PORT" > "$E2E_DIR/mock-api.log" 2>&1 &
MOCK_PID=$!
mock_ready=0
for _ in $(seq 1 40); do
  if curl -sf -o /dev/null "$API/__requests"; then
    mock_ready=1
    break
  fi
  sleep 0.25
done
if [ "$mock_ready" != 1 ]; then
  echo "e2e-anonymous: the mock API did not become ready; log:" >&2
  cat "$E2E_DIR/mock-api.log" >&2
  exit 1
fi

echo "== e2e-anonymous: building the web app (API origin = the mock, baked in) =="
(
  cd "$WEB_DIR"
  pnpm run build
)

echo "== e2e-anonymous: starting next on $BASE =="
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
  echo "e2e-anonymous: server did not become ready; log:" >&2
  cat "$E2E_DIR/server.log" >&2
  exit 1
fi
echo "== e2e-anonymous: server ready =="

echo "== e2e-anonymous: anonymous checklist =="
if ! node "$E2E_DIR/anonymous-e2e.mjs" "$BASE" "$API"; then
  echo "e2e-anonymous: FAILED (server log: $E2E_DIR/server.log; mock log: $E2E_DIR/mock-api.log)" >&2
  exit 1
fi
echo "e2e-anonymous: all passed"
