#!/usr/bin/env bash
# Web visual + a11y smoke (T0107 required tests): builds the web app,
# serves it with `next start`, and runs the two browser checklists against
# it in real Chromium. Self-contained: the harness deps live in this
# directory's own npm project (its own lockfile), outside the pnpm
# workspace, so the repository's pnpm-lock.yaml is never touched.
#
# Usage: bash tests/web-smoke/run.sh
# Env:   WEB_SMOKE_PORT (default 31107) overrides the server port.
set -euo pipefail

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SMOKE_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
PORT="${WEB_SMOKE_PORT:-31107}"
BASE="http://127.0.0.1:$PORT"

# The web app fails closed without a validated config (instrumentation.ts);
# these values mirror the G2/G4 CI env. The smoke pages render with the
# services "down" when they are not running — the nav under test is static.
export POST_ENV=prod
export API_BASE_URL=http://127.0.0.1:18080
export SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100

SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ]; then
    # Kill the server and any children it may have spawned.
    pkill -P "$SERVER_PID" 2>/dev/null || true
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Never test against a stale server: if the port is already taken (e.g. a
# previous run's process outlived its wrapper), fail loudly instead of
# silently exercising someone else's build.
if ss -tln 2>/dev/null | grep -qE ":$PORT[[:space:]]"; then
  echo "web-smoke: port $PORT is already in use; a previous run may have leaked a server" >&2
  exit 1
fi

echo "== web-smoke: installing harness deps (own lockfile, npm) =="
(
  cd "$SMOKE_DIR"
  npm install --no-audit --no-fund
  # Chromium headless shell only (the full build needs more system libs and
  # headed mode is not used). Host validation is skipped because on this
  # machine the browser runs against the locally extracted libs (see
  # bootstrap-deps.sh); on hosts with the system libs it is a no-op either
  # way — the browser really does launch before any test is trusted.
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

echo "== web-smoke: browser library bootstrap =="
LIB_PATH="$(bash "$SMOKE_DIR/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

echo "== web-smoke: building the web app =="
(
  cd "$WEB_DIR"
  pnpm run build
)

echo "== web-smoke: starting next on $BASE =="
(
  cd "$WEB_DIR"
  # Invoke the next binary directly (not through npx): the backgrounded
  # process is then the server itself, so the trap above can reliably kill
  # it instead of orphaning the real listener behind an npx wrapper.
  "$WEB_DIR/node_modules/.bin/next" start -p "$PORT"
) > "$SMOKE_DIR/server.log" 2>&1 &
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
  echo "web-smoke: server did not become ready; log:" >&2
  cat "$SMOKE_DIR/server.log" >&2
  exit 1
fi
echo "== web-smoke: server ready =="

rc=0
echo "== web-smoke: visual smoke =="
if ! node "$SMOKE_DIR/visual-smoke.mjs" "$BASE" "$WEB_DIR"; then
  rc=1
fi
echo "== web-smoke: a11y smoke =="
if ! node "$SMOKE_DIR/a11y-smoke.mjs" "$BASE"; then
  rc=1
fi

if [ "$rc" != 0 ]; then
  echo "web-smoke: FAILED (server log: $SMOKE_DIR/server.log)" >&2
else
  echo "web-smoke: visual + a11y smoke all passed"
fi
exit "$rc"
