#!/usr/bin/env bash
# PR Research State Diff UI e2e (T0408 required test "pr ui e2e"): builds
# the web app, serves it with `next start`, and drives the pull request
# detail page in real Chromium against a network-layer mock of the Go API
# (the exact wire shapes cmd/api/pullrequestshttp, cmd/api/reviewhttp and
# internal/rsg/diff produce — the Go handler suites lock those shapes
# against the real guard). Self-contained like tests/e2e-conflicts: the
# harness deps live in this directory's own npm project (its own
# lockfile), outside the pnpm workspace, so the repository's
# pnpm-lock.yaml is never touched.
#
# Usage: bash tests/e2e-pulls/run.sh
# Env:   PULLS_E2E_PORT (default 31120) overrides the server port.
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$E2E_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
PORT="${PULLS_E2E_PORT:-31120}"
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
  echo "e2e-pulls: port $PORT is already in use; a previous run may have leaked a server" >&2
  exit 1
fi

echo "== e2e-pulls: installing harness deps (own lockfile, npm) =="
(
  cd "$E2E_DIR"
  npm install --no-audit --no-fund
  # Chromium headless shell only. The browsers are already in the shared
  # Playwright cache; this call is a no-op when they are.
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

echo "== e2e-pulls: browser library bootstrap (shared web-smoke helper) =="
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

echo "== e2e-pulls: building the web app (API origin baked in) =="
(
  cd "$WEB_DIR"
  pnpm run build
)

echo "== e2e-pulls: starting next on $BASE =="
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
  echo "e2e-pulls: server did not become ready; log:" >&2
  cat "$E2E_DIR/server.log" >&2
  exit 1
fi
echo "== e2e-pulls: server ready =="

echo "== e2e-pulls: PR Research State Diff UI =="
if ! node "$E2E_DIR/pulls-e2e.mjs" "$BASE" "$WEB_DIR"; then
  echo "e2e-pulls: FAILED (server log: $E2E_DIR/server.log)" >&2
  exit 1
fi
echo "e2e-pulls: all passed"
