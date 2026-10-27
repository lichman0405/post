#!/usr/bin/env bash
# WEB-01 required test: "removing a primary button handler turns the
# corresponding journey red".
#
# This is the anti-bypass proof docs/70 §6.4 asks for. The journey can only
# be trusted if it fails when the product control it claims to use stops
# working. So this script:
#
#   1. disconnects the primary submit button's handler in the REAL login card
#      (the form's onSubmit no longer calls submit(); the button remains
#      present, visible, enabled and clickable — only its effect is gone);
#   2. runs the real harness and requires the journey to FAIL on the step
#      that needs the button, with the reason naming the primary button;
#   3. restores the source in a trap (also on INT/TERM), verifies the exact
#      anchor is back, and requires a clean run to PASS.
#
# The mutation leaves the button in the DOM on purpose: a removed element
# would be caught by a selector search, which proves less than a button that
# looks right and does nothing. Restoring before the final run also makes
# "the harness is green on the unmutated tree" part of the same evidence.
#
# Usage: bash tests/web-v2/mutation-check.sh
#   Env: WEB_V2_MUTATION_EVIDENCE — root for the two runs' evidence
#        (default tests/web-v2/evidence/mutation-<timestamp>)
set -euo pipefail

V2_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$V2_DIR/../.." && pwd)"
TARGET="$ROOT/apps/web/app/(auth)/login/login-card.tsx"
ORIGINAL='          void submit();'
MUTATED='          /* WEB-01 mutation-check: the primary submit handler is disconnected on purpose */'
RUN_ROOT="${WEB_V2_MUTATION_EVIDENCE:-$V2_DIR/evidence/mutation-$(date -u +%Y%m%dT%H%M%SZ)-$$}"

needle_count() { # file needle
  grep -cF -- "$2" "$1" 2>/dev/null || true
}

restore() {
  if [ ! -f "$TARGET" ]; then
    return
  fi
  if [ "$(needle_count "$TARGET" "$MUTATED")" -gt 0 ]; then
    MUTATED="$MUTATED" ORIGINAL="$ORIGINAL" perl -0pi -e 's/\Q$ENV{MUTATED}\E/$ENV{ORIGINAL}/' "$TARGET"
    echo "mutation-check: restored $TARGET" >&2
  fi
}
# The product source is mutated while this runs; an interrupted run must not
# leave it that way.
trap restore EXIT INT TERM

fail() {
  echo "mutation-check: FAILED — $1" >&2
  exit 1
}

[ -f "$TARGET" ] || fail "$TARGET does not exist; the mutation anchor has moved"
[ "$(needle_count "$TARGET" "$ORIGINAL")" -eq 1 ] || fail "expected exactly one primary-handler anchor in $TARGET (found $(needle_count "$TARGET" "$ORIGINAL")); update this script's anchor rather than skipping the proof"
[ "$(needle_count "$TARGET" "$MUTATED")" -eq 0 ] || fail "$TARGET already carries the mutation; refusing to draw a conclusion from a dirty tree"
git -C "$ROOT" diff --quiet -- "$TARGET" || fail "$TARGET has uncommitted changes before the check; restore it first"

mkdir -p "$RUN_ROOT"

echo "== mutation-check: arming — disconnecting the primary submit handler =="
MUTATED="$MUTATED" ORIGINAL="$ORIGINAL" perl -0pi -e 's/\Q$ENV{ORIGINAL}\E/$ENV{MUTATED}/' "$TARGET"
[ "$(needle_count "$TARGET" "$MUTATED")" -eq 1 ] || fail "the mutation was not armed; refusing to run"
[ "$(needle_count "$TARGET" "$ORIGINAL")" -eq 0 ] || fail "the original handler survived next to the mutation"

echo "== mutation-check: mutated run (expect the journey to FAIL) =="
set +e
mutated_output="$(
  WEB_V2_EVIDENCE_DIR="$RUN_ROOT/mutated" \
  WEB_V2_RUN_ID="mutation-$(date -u +%Y%m%dT%H%M%SZ)-$$" \
  bash "$V2_DIR/run.sh" 2>&1
)"
mutated_status=$?
set -e
echo "$mutated_output" | tail -40
echo "mutation-check: mutated run exited $mutated_status (0 would be the bug)"

[ "$mutated_status" -ne 0 ] || fail "BUG — the harness passed while the primary submit handler was disconnected"
if grep -q "the web app production build failed" <<<"$mutated_output"; then
  fail "the mutated run failed at BUILD time, not at the journey; a build error does not prove the journey depends on the button"
fi
if ! grep -q "sign-up: primary button did not reach a signed-in home page" <<<"$mutated_output"; then
  fail "the mutated run failed, but not on the primary-button assertion (expected 'sign-up: primary button did not reach a signed-in home page')"
fi
echo "mutation-check: the journey caught the disconnected primary button for the expected reason"

echo "== mutation-check: restoring the handler =="
restore
[ "$(needle_count "$TARGET" "$ORIGINAL")" -eq 1 ] || fail "the revert did not take; $TARGET is left mutated"
[ "$(needle_count "$TARGET" "$MUTATED")" -eq 0 ] || fail "the mutated anchor is still present after the revert"
git -C "$ROOT" diff --quiet -- "$TARGET" || fail "the revert left $TARGET different from its baseline"

echo "== mutation-check: restored run (expect PASS) =="
set +e
restored_output="$(
  WEB_V2_EVIDENCE_DIR="$RUN_ROOT/restored" \
  WEB_V2_RUN_ID="restored-$(date -u +%Y%m%dT%H%M%SZ)-$$" \
  bash "$V2_DIR/run.sh" 2>&1
)"
restored_status=$?
set -e
echo "$restored_output" | tail -30
[ "$restored_status" -eq 0 ] || fail "BUG — the harness is red on the restored tree, so the mutated red proved nothing"

cat > "$RUN_ROOT/mutation-check.json" <<EOF
{
  "check": "primary submit handler disconnected -> journey red; restored -> green",
  "target": "apps/web/app/(auth)/login/login-card.tsx",
  "mutated_run_exit": $mutated_status,
  "restored_run_exit": $restored_status,
  "observed_failure": "sign-up: primary button did not reach a signed-in home page",
  "reverted": true,
  "result": "passed"
}
EOF

echo "mutation-check: PASSED — the mutated tree was red for the button, the restored tree was green (evidence: $RUN_ROOT)"
