#!/usr/bin/env bash
# Web visual + a11y smoke (T0107 required tests): builds the web app,
# serves it with `next start`, and runs the browser checklists against it in
# real Chromium. Self-contained: the harness deps live in this directory's
# own npm project (its own lockfile), outside the pnpm workspace, so the
# repository's pnpm-lock.yaml is never touched.
#
# T1112 — THE CHECK LIST IS DISCOVERED, NOT WRITTEN DOWN. Every *.mjs file
# in this directory IS a check; the runner finds them, drives each with one
# argument (the base URL) and reports them one by one. A list of names typed
# into a runner is the exact shape of the failure T1112 exists to remove:
# T1107's tests/e2e-shell was red for six days because the only places that
# could have named it were lists somebody had to remember to update. A check
# added here is run by `make browser-smoke` the day it lands; a check that
# disappears cannot linger as a name matching nothing.
#
# A check's NAME is its file name with `.mjs` and a trailing `-smoke`
# removed: visual-smoke.mjs -> visual, a11y-smoke.mjs -> a11y (the same
# names package.json's scripts use).
#
# Usage: bash tests/web-smoke/run.sh
# Env:   WEB_SMOKE_PORT    (default 31107) overrides the server port.
#        WEB_SMOKE_CHECKS  comma-separated check names (default: every
#                          discovered check), e.g. WEB_SMOKE_CHECKS=visual
#        WEB_SMOKE_WEB_DIR overrides the web app directory visual-smoke
#                          reads its CSS policy files from.
set -euo pipefail

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SMOKE_DIR/../.." && pwd)"
WEB_DIR="${WEB_SMOKE_WEB_DIR:-$ROOT/apps/web}"
PORT="${WEB_SMOKE_PORT:-31107}"
BASE="http://127.0.0.1:$PORT"

# ---- Discovery ----------------------------------------------------------
# First, and before the prerequisite checks below: `--list` is a question
# about this directory, not a request to run anything, and the Makefile asks
# it on every invocation to build its help and its `browser-list`. Answering
# it only after node_modules exists would make a fresh clone report that this
# harness has no checks — a silent omission produced by ordering, which is
# the family of bug this task exists to remove.
declare -A CHECK_FILE=()
CHECKS=()
for file in "$SMOKE_DIR"/*.mjs; do
  if [ ! -e "$file" ]; then
    echo "web-smoke: FAILED — no *.mjs checks under $SMOKE_DIR; this harness has nothing to run." >&2
    exit 1
  fi
  name="$(basename "$file" .mjs)"
  name="${name%-smoke}"
  if [ -n "${CHECK_FILE[$name]:-}" ]; then
    echo "web-smoke: FAILED — check name collision: both '${CHECK_FILE[$name]}' and '$file' map to name '$name'; rename one of them." >&2
    exit 1
  fi
  CHECK_FILE["$name"]="$file"
  CHECKS+=("$name")
done

if [ "${1:-}" = "--list" ]; then
  printf '%s\n' "${CHECKS[@]}"
  exit 0
fi

# Declare what is missing rather than discovering it halfway through a build
# in a form the reader has to decode. A skip is not a pass, so every one of
# these is a failure with the reason and the remedy.
if ! command -v node >/dev/null 2>&1; then
  echo "web-smoke: FAILED — node is not on PATH; the browser checks cannot run and a skip is not a pass." >&2
  exit 1
fi
# Static arity check first, and before the web build: a two-argument ok() is
# a call that can never fail, and it costs 50ms to catch it here rather than
# after an 80-second build and a browser launch. The runtime guard inside ok
# still catches any that execute; this catches the ones that would not.
if ! node "$SMOKE_DIR/check-ok-arity.js"; then
  echo "web-smoke: FAILED — a check that cannot fail was found in this directory (see ok-arity above)." >&2
  exit 1
fi
if [ ! -d "$WEB_DIR/node_modules" ]; then
  echo "web-smoke: FAILED — $WEB_DIR/node_modules is missing and this harness builds the web app. Run 'pnpm install --frozen-lockfile' at the repository root first." >&2
  exit 1
fi

SELECTED=("${CHECKS[@]}")
if [ -n "${WEB_SMOKE_CHECKS:-}" ]; then
  SELECTED=()
  IFS=',' read -r -a requested <<<"${WEB_SMOKE_CHECKS}"
  for want in "${requested[@]}"; do
    want="${want// /}"
    [ -n "$want" ] || continue
    if [ -z "${CHECK_FILE[$want]:-}" ]; then
      # A name that matches nothing is a configuration error, not a smaller
      # run: silently running fewer checks is how "green" stops meaning
      # "checked".
      echo "web-smoke: FAILED — '$want' is not a check in this directory; discovered: ${CHECKS[*]}" >&2
      exit 1
    fi
    SELECTED+=("$want")
  done
fi

# "Zero checks ran" must never read as "everything passed".
if [ "${#SELECTED[@]}" -eq 0 ]; then
  echo "web-smoke: FAILED — WEB_SMOKE_CHECKS selected no checks (discovered: ${CHECKS[*]}); an empty run is not a green run." >&2
  exit 1
fi

echo "web-smoke: discovered ${#CHECKS[@]} check(s): ${CHECKS[*]}"
echo "web-smoke: running ${#SELECTED[@]}: ${SELECTED[*]}"

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

# ---- Run ----------------------------------------------------------------
# Each check runs under its own heading so a failure is located, not just
# counted, and the loop keeps going so one red check cannot hide the next.
PASSED=()
FAILED=()
for name in "${SELECTED[@]}"; do
  echo ""
  echo "---- web-smoke: $name ----"
  if node "${CHECK_FILE[$name]}" "$BASE"; then
    PASSED+=("$name")
  else
    rc=$?
    FAILED+=("$name")
    echo "web-smoke: check $name FAILED (exit $rc)" >&2
  fi
done

echo ""
echo "== web-smoke: summary =="
for name in "${PASSED[@]}"; do echo "  ok   $name"; done
for name in "${FAILED[@]}"; do echo "  FAIL $name"; done

if [ "${#FAILED[@]}" -ne 0 ]; then
  echo "web-smoke: FAILED — checks: ${FAILED[*]} (server log: $SMOKE_DIR/server.log)" >&2
  exit 1
fi
echo "web-smoke: all ${#PASSED[@]} check(s) passed"
