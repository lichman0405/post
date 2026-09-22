#!/usr/bin/env bash
# Visual regression (T1101 required test "visual regression"): builds the
# web app against a mocked API origin, serves it with `next start`, renders
# the core pages in real Chromium at a fixed viewport, and compares each
# screenshot against the checked-in baselines in tests/web-smoke/baseline/.
# It lives beside the web-smoke harness, whose own npm project it shares —
# outside the pnpm workspace, so the repository's pnpm-lock.yaml is never
# touched.
#
# Why this is not just "another smoke test": tests/web-smoke/visual-smoke.mjs
# writes its screenshots as ARTIFACTS and asserts nothing about them, so no
# CSS change can ever turn it red. This run's comparison has a fail
# condition, and visual-regression-mutation-check.sh proves it can reach it.
#
# Usage: bash tests/web-smoke/visual-regression-run.sh
# Env:   VISUAL_REGRESSION_PORT (default 31150) overrides the server port.
#        UPDATE_BASELINE=1 regenerates the baselines instead of comparing —
#        it compares nothing, so it is never what CI runs.
set -euo pipefail

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SMOKE_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
PORT="${VISUAL_REGRESSION_PORT:-31150}"
BASE="http://127.0.0.1:$PORT"

# The API origin is baked in at BUILD time, so it has to be the origin the
# Playwright router intercepts — no DNS is involved in any API "call".
export POST_ENV=prod
export API_BASE_URL=http://api.e2e.test
export SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100

SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ]; then
    pkill -P "$SERVER_PID" 2>/dev/null || true
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

if ss -tln 2>/dev/null | grep -qE ":$PORT[[:space:]]"; then
  echo "visual-regression: port $PORT is already in use; a previous run may have leaked a server" >&2
  exit 1
fi

echo "== visual-regression: installing harness deps (own lockfile, npm) =="
(
  cd "$SMOKE_DIR"
  npm install --no-audit --no-fund
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

echo "== visual-regression: browser library bootstrap (shared web-smoke helper) =="
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

echo "== visual-regression: building the web app (API origin baked in) =="
(
  cd "$WEB_DIR"
  pnpm run build
)

echo "== visual-regression: starting next on $BASE =="
(
  cd "$WEB_DIR"
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
  echo "visual-regression: server did not become ready; log:" >&2
  cat "$SMOKE_DIR/server.log" >&2
  exit 1
fi
echo "== visual-regression: server ready =="

echo "== visual-regression: render + compare =="
if ! node "$SMOKE_DIR/visual-regression.mjs" "$BASE" "$ROOT"; then
  echo "visual-regression: FAILED (server log: $SMOKE_DIR/server.log)" >&2
  exit 1
fi
echo "visual-regression: all passed"
