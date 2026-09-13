#!/usr/bin/env bash
# Mutation check for the settings e2e harness itself: prove the checklist
# fails — on the RIGHT assertion — when the implementation (here: the
# mock's CSRF answer) drifts. Mutates only the harness file, runs the
# checklist against an already built + served app, then restores the
# file. The caller must export LD_LIBRARY_PATH (tests/web-smoke/
# bootstrap-deps.sh) and PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1.
# Usage:
#   bash tests/e2e-settings/mutation-check.sh <base-url> [apps-web-dir]
set -euo pipefail

BASE="${1:-http://127.0.0.1:31109}"
APPS_WEB="${2:-apps/web}"
E2E="tests/e2e-settings/settings-e2e.mjs"

if ! curl -sf -o /dev/null "$BASE/"; then
  echo "mutation-check: $BASE is not serving; start the app first" >&2
  exit 1
fi

cp "$E2E" /tmp/settings-e2e.mjs.bak
sed -i 's/csrf_token: "e2e-csrf-token"/csrf_token: "e2e-wrong-token"/' "$E2E"

echo "--- mutated run (expect FAIL on the CSRF wire assertion) ---"
set +e
mutated_out="$(node "$E2E" "$BASE" "$APPS_WEB" 2>&1)"
mutated_status=$?
set -e
if [ "$mutated_status" -eq 0 ]; then
  echo "mutation-check: BUG — the harness passed against a wrong CSRF token" >&2
  cp /tmp/settings-e2e.mjs.bak "$E2E"
  exit 1
fi
if ! grep -qE "^FAIL .*(PUT|PATCH) wire.*e2e-wrong-token" <<<"$mutated_out"; then
  echo "mutation-check: BUG — the harness failed, but not on the CSRF wire assertion:" >&2
  grep -E "^FAIL" <<<"$mutated_out" | head -5 >&2 || true
  echo "$mutated_out" | tail -5 >&2
  cp /tmp/settings-e2e.mjs.bak "$E2E"
  exit 1
fi
echo "mutation-check: harness failed on the CSRF wire assertion as expected"

cp /tmp/settings-e2e.mjs.bak "$E2E"

echo "--- restored run (expect all pass) ---"
if ! node "$E2E" "$BASE" "$APPS_WEB"; then
  echo "mutation-check: BUG — the harness fails after restoration" >&2
  exit 1
fi
echo "mutation-check: restored harness passes; the checklist is real"
