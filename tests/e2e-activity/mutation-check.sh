#!/usr/bin/env bash
# Mutation check for the activity e2e harness AND the page it drives: break
# the source filter's refusal — an unknown ?source= is silently read as
# "everything" instead of being refused — then rebuild and prove the
# checklist fails, on the RIGHT assertion, and passes again once the break is
# reverted.
#
# The mutation is applied to apps/web/lib/activity.ts, NOT to the checklist:
# mutating the harness would only prove the harness can disagree with
# itself. The broken line is the one place where a refused filter value is
# turned away, so the mutated build answers a question the reader did not
# ask — exactly the defect the refusal checks exist for.
#
# Usage: bash tests/e2e-activity/mutation-check.sh
# Env:   ACTIVITY_MUTATION_PORT (default 31111) — kept off run.sh's default
#        port so a normal run and a mutation run never share a listener.
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$E2E_DIR/../.." && pwd)"
PORT="${ACTIVITY_MUTATION_PORT:-31111}"

cd "$ROOT"
LIB="apps/web/lib/activity.ts"
BACKUP="/tmp/t0607-activity-mutation.bak"

cp "$LIB" "$BACKUP"
restore() {
  cp "$BACKUP" "$LIB"
}
trap restore EXIT

# The throw that refuses an unknown value becomes a silent "read both".
sed -i 's|^  throw new Error..unknown activity source.*|  return null; // MUTATION: a refused filter is silently widened|' "$LIB"
if ! grep -q "MUTATION" "$LIB"; then
  echo "mutation-check: BUG — the mutation did not apply (has the refusal moved?)" >&2
  exit 1
fi
echo "mutation-check: mutated $LIB: an unknown ?source= now reads both registries"

echo "--- mutated run (expect FAIL on the refusal assertions) ---"
set +e
mutated_out="$(ACTIVITY_E2E_PORT="$PORT" bash "$E2E_DIR/run.sh" 2>&1)"
mutated_status=$?
set -e
restore
if [ "$mutated_status" -eq 0 ]; then
  echo "mutation-check: BUG — the checklist passed against a page that widened a refused filter" >&2
  exit 1
fi
if ! grep -qE "^FAIL refusal: an unknown \?source= renders the page's own refusal, with no feed request" <<<"$mutated_out"; then
  echo "mutation-check: BUG — the checklist failed, but not on the refusal assertion:" >&2
  grep -E "^FAIL|Error|error:" <<<"$mutated_out" | head -10 >&2 || true
  exit 1
fi
echo "mutation-check: the checklist failed on the refusal assertion, as it must:"
grep -E "^FAIL" <<<"$mutated_out" | head -5

echo "--- restored run (expect all pass) ---"
if ! ACTIVITY_E2E_PORT="$PORT" bash "$E2E_DIR/run.sh" 2>&1 | tail -45; then
  echo "mutation-check: BUG — the checklist fails after restoration" >&2
  exit 1
fi
echo "mutation-check: restored page passes; the checklist is real"
