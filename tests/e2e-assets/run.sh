#!/usr/bin/env bash
# Asset hub e2e (T0709 required test "asset ui e2e"): builds the web app,
# serves it with `next start`, and drives /assets and one asset's page in
# real Chromium against a network-layer mock of the Go API (the exact wire
# shapes cmd/api/assetshttp + internal/assets.BuildPage produce — the Go
# e2e (tests/e2e/asset_page_e2e_test.go) and the integration suite
# (tests/integration/asset_page_test.go) lock those shapes against the real
# guard and real PostgreSQL). Self-contained like tests/e2e-files: the
# harness deps live in this directory's own npm project (its own lockfile),
# outside the pnpm workspace, so the repository's pnpm-lock.yaml is never
# touched.
#
# Usage: bash tests/e2e-assets/run.sh
# Env:   ASSETS_E2E_PORT (default 31121) overrides the server port.
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$E2E_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
PORT="${ASSETS_E2E_PORT:-31121}"
BASE="http://127.0.0.1:$PORT"

# The API origin the web app fetches is baked in at build time; the
# Playwright mock intercepts this exact origin, so no DNS/network is
# involved in the API "calls".
export POST_ENV=prod
export API_BASE_URL=http://api.e2e.test
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
  echo "e2e-assets: port $PORT is already in use; a previous run may have leaked a server" >&2
  exit 1
fi

echo "== e2e-assets: installing harness deps (own lockfile, npm) =="
(
  cd "$E2E_DIR"
  npm install --no-audit --no-fund
  # Chromium headless shell only. The browsers are already in the shared
  # Playwright cache; this call is a no-op when they are.
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

echo "== e2e-assets: browser library bootstrap (shared web-smoke helper) =="
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

echo "== e2e-assets: building the web app (API origin baked in) =="
(
  cd "$WEB_DIR"
  pnpm run build
)

echo "== e2e-assets: starting next on $BASE =="
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
  echo "e2e-assets: server did not become ready; log:" >&2
  cat "$E2E_DIR/server.log" >&2
  exit 1
fi
echo "== e2e-assets: server ready =="

echo "== e2e-assets: asset hub checklist =="
if ! node "$E2E_DIR/assets-e2e.mjs" "$BASE" "$WEB_DIR"; then
  echo "e2e-assets: FAILED (server log: $E2E_DIR/server.log)" >&2
  exit 1
fi
echo "e2e-assets: all passed"
