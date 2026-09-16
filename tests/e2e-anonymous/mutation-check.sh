#!/usr/bin/env bash
# Mutation check for the anonymous-pages e2e harness (T0801 required test
# "anonymous e2e"): prove the checklist FAILS — on the right assertion —
# when the thing it protects is broken, and passes again once it is not.
#
# The mutation is the worst realistic bug this task exists to prevent: an
# API that answers an anonymous caller for a PRIVATE entity. It is applied
# to the mock, not to the app (the app is untouched, so the pages under
# test are the real ones), by starting the mock with MOCK_LEAK_PRIVATE=1 —
# see mock-api.mjs. If the checklist still passed under it, the checklist
# would be measuring nothing: every "private 不索引" assertion in it would
# be a claim about a fixture that was never exercised.
#
# The web app must already be built and serving (run.sh leaves it in that
# state inside its own run; this script expects a server started the same
# way), because the point is to re-read REAL pages, not to rebuild them.
#
# Usage:
#   ANON_E2E_PORT=31141 ANON_E2E_MOCK_PORT=31142 bash tests/e2e-anonymous/mutation-check.sh [base-url] [api-url]
# The caller must export LD_LIBRARY_PATH (tests/web-smoke/bootstrap-deps.sh)
# and PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1.
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE="${1:-http://127.0.0.1:${ANON_E2E_PORT:-31141}}"
API="${2:-http://127.0.0.1:${ANON_E2E_MOCK_PORT:-31142}}"
MOCK_PORT="${API##*:}"

if ! curl -sf -o /dev/null "$BASE/"; then
  echo "mutation-check: $BASE is not serving; build and start the app first" >&2
  exit 1
fi
if ss -tln 2>/dev/null | grep -qE ":$MOCK_PORT[[:space:]]"; then
  echo "mutation-check: port $MOCK_PORT is already in use; stop the other mock first" >&2
  exit 1
fi

MOCK_PID=""
stop_mock() {
  if [ -n "$MOCK_PID" ]; then
    kill "$MOCK_PID" 2>/dev/null || true
    wait "$MOCK_PID" 2>/dev/null || true
    MOCK_PID=""
  fi
}
trap stop_mock EXIT

start_mock() {
  MOCK_LEAK_PRIVATE="${1:-0}" node "$E2E_DIR/mock-api.mjs" "$MOCK_PORT" \
    > "$E2E_DIR/mock-api.log" 2>&1 &
  MOCK_PID=$!
  for _ in $(seq 1 40); do
    if curl -sf -o /dev/null "$API/__requests"; then
      return 0
    fi
    sleep 0.25
  done
  echo "mutation-check: the mock did not become ready; log:" >&2
  cat "$E2E_DIR/mock-api.log" >&2
  exit 1
}

echo "== mutation-check: starting the mock with MOCK_LEAK_PRIVATE=1 (it will lie) =="
start_mock 1
if ! grep -q "MOCK_LEAK_PRIVATE=1" "$E2E_DIR/mock-api.log"; then
  echo "mutation-check: the mutation was not armed; refusing to draw a conclusion" >&2
  exit 1
fi

echo "--- mutated run (expect FAIL on the private-entity assertions) ---"
set +e
mutated_out="$(node "$E2E_DIR/anonymous-e2e.mjs" "$BASE" "$API" 2>&1)"
mutated_status=$?
set -e
echo "$mutated_out" | grep -E "^FAIL|^anonymous-e2e" | head -20

if [ "$mutated_status" -eq 0 ]; then
  echo "mutation-check: BUG — the checklist passed while the API served a private entity" >&2
  exit 1
fi
for assertion in \
  '^FAIL private project: it says noindex, nofollow' \
  '^FAIL private project: the served HTML never contains' \
  '^FAIL private project: it carries the generic, name-free title'
do
  if ! grep -qE "$assertion" <<<"$mutated_out"; then
    echo "mutation-check: BUG — the checklist failed, but not on $assertion" >&2
    exit 1
  fi
done
echo "mutation-check: the checklist caught the leak and named it"
stop_mock

echo "--- restored run (expect all pass) ---"
start_mock 0
if ! node "$E2E_DIR/anonymous-e2e.mjs" "$BASE" "$API"; then
  echo "mutation-check: BUG — the checklist fails against an honest API" >&2
  exit 1
fi
echo "mutation-check: the honest API passes; the checklist is real"
