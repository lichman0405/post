#!/usr/bin/env bash
# T0608 required test "playwright release flow": project policy → research
# pull request → review → merge → release → asset publish → object abort →
# the old release and the old asset version unchanged — driven in real
# Chromium against the REAL stack. The Go harness in tests/e2e-release/
# harness composes the production handlers, stores and guard over a real
# PostgreSQL; the web app is the built app under `next start`; the browser
# talks to the API itself (real CORS preflights, real session cookie, real
# CSRF token, real Idempotency-Key). Nothing is mocked.
#
# Unlike tests/e2e-pr-flows this suite needs no git provider: the flow
# pushes no ref and the merge is wired without one, exactly as
# tests/integration/abort_e2e_test.go wires it. PostgreSQL is the whole
# dependency (plus the browser's own libraries, bootstrapped below).
#
# The suite is TWO scripts over one stack: release-e2e.mjs (the chain) and
# publish-ux-e2e.mjs (T1103's publish/visibility confirmation page, driven
# against the release the chain just made). Both are run below; see the note
# at the invocations for why they are one suite and not two.
#
# Usage: bash tests/e2e-release/run.sh
# Env:   RELEASE_WEB_PORT (default 31170)   the web app's port
#        RELEASE_API_PORT (default 18191)   the harness API's port
#        POSTGRES_TEST_ADMIN_URL            as the integration suite
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$E2E_DIR/../.." && pwd)"
WEB_DIR="$ROOT/apps/web"
WEB_PORT="${RELEASE_WEB_PORT:-31170}"
API_PORT="${RELEASE_API_PORT:-18191}"
BASE="http://127.0.0.1:$WEB_PORT"
API="http://127.0.0.1:$API_PORT"

export POSTGRES_TEST_ADMIN_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
# The harness reads its own two variables (it takes no flags).
export RELEASE_API_ADDR="127.0.0.1:$API_PORT"
export RELEASE_WEB_ORIGIN="$BASE"

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
    echo "e2e-release: port $port is already in use; a previous run may have leaked a server" >&2
    exit 1
  fi
done

echo "== e2e-release: installing harness deps (own lockfile, npm) =="
(
  cd "$E2E_DIR"
  npm install --no-audit --no-fund
  # Chromium headless shell only; a no-op when the shared cache has it.
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1 npx playwright install chromium --only-shell
)

echo "== e2e-release: browser library bootstrap (shared web-smoke helper) =="
LIB_PATH="$(bash "$ROOT/tests/web-smoke/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

echo "== e2e-release: building the API harness =="
(
  cd "$ROOT"
  go build -o "$WORK_DIR/release-harness" ./tests/e2e-release/harness
)

echo "== e2e-release: building the web app (API origin baked in) =="
(
  cd "$WEB_DIR"
  pnpm run build
)

echo "== e2e-release: starting the API harness on $API =="
"$WORK_DIR/release-harness" > "$WORK_DIR/harness.log" 2>&1 &
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
    echo "e2e-release: the API harness exited before it was ready; log:" >&2
    cat "$WORK_DIR/harness.log" >&2
    exit 1
  fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "e2e-release: the API harness did not become ready; log:" >&2
  cat "$WORK_DIR/harness.log" >&2
  exit 1
fi
echo "== e2e-release: the API harness is ready (own database, real PostgreSQL) =="

echo "== e2e-release: starting next on $BASE =="
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
  echo "e2e-release: the web app did not become ready; log:" >&2
  cat "$WORK_DIR/web.log" >&2
  exit 1
fi
echo "== e2e-release: web app ready =="

# Two scripts, one stack, in order. The publish/visibility confirmation suite
# (T1103, required test "publish ux e2e") drives the SAME release and object
# version this chain has just created — a confirmation page about a release
# needs a release — so it runs here, immediately after the chain that made
# one, against the web app and harness already running. It takes that state as
# its third argument (the chain file below), which is also the only thing that
# ties the two together: neither script knows the other exists.
#
# It used to live in a second launcher (tests/e2e-release/publish-ux-run.sh)
# with its own ports (31171/18192). That was the suite's evidence sitting
# outside every discovered runner: `make browser-e2e` walks tests/e2e-*/run.sh,
# so a suite whose only launcher is a differently-named script is a suite CI
# never runs. The two ports were never the point — one stack hosts both
# scripts, and the chain file is how the second learns what the first did.
chain_json="$WORK_DIR/chain.json"
echo "== e2e-release: the flow, in Chromium =="
if ! RELEASE_CHAIN_OUT="$chain_json" RELEASE_SHOT_DIR="$WORK_DIR" node "$E2E_DIR/release-e2e.mjs" "$BASE" "$ready_json"; then
  echo "e2e-release: FAILED (harness log: $WORK_DIR/harness.log, web log: $WORK_DIR/web.log)" >&2
  tail -40 "$WORK_DIR/harness.log" >&2 || true
  exit 1
fi

echo "== e2e-release: the publish/visibility confirmation page, in Chromium =="
if ! node "$E2E_DIR/publish-ux-e2e.mjs" "$BASE" "$ready_json" "$chain_json"; then
  echo "e2e-release: publish-ux FAILED (harness log: $WORK_DIR/harness.log, web log: $WORK_DIR/web.log)" >&2
  tail -40 "$WORK_DIR/harness.log" >&2 || true
  exit 1
fi
echo "e2e-release: all passed"
