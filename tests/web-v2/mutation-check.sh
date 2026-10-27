#!/usr/bin/env bash
# WEB-01 required test: "removing a primary button handler turns the
# corresponding journey red".
#
# This is the anti-bypass proof docs/70 §6.4 asks for. The journey can only
# be trusted if it fails when the product control it claims to use stops
# working. So this script:
#
#   1. snapshots the target file's CURRENT bytes;
#   2. disconnects the primary submit button's handler in the REAL login card
#      (the form's onSubmit no longer calls submit(); the button remains
#      present, visible, enabled and clickable — only its effect is gone);
#   3. runs the real harness and requires the journey to FAIL on the step
#      that needs the button, with the reason naming the primary button;
#   4. restores the exact snapshot in a trap (also on INT/TERM), verifies the
#      bytes are identical to what was there before, and requires a clean run
#      to PASS.
#
# WHY IT SNAPSHOTS INSTEAD OF REQUIRING A CLEAN GIT TREE
# -----------------------------------------------------
# This check runs on the G2 integration/task tree, where the task's diff is
# deliberately UNCOMMITTED (Workers may not commit). Refusing a dirty target
# would make the required job red on every Worker branch for a property the
# proof does not depend on: what matters is that the tree is byte-identical
# after the restore, not that it matched HEAD before the run. So the proof is
# anchored to a snapshot of the current bytes; it refuses only when that
# snapshot cannot be taken (or the mutation anchor itself is not present).
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
SNAPSHOT="$RUN_ROOT/target-snapshot.bak"

needle_count() { # file needle
  grep -cF -- "$2" "$1" 2>/dev/null || true
}

sha_of() { # file
  sha256sum "$1" | awk '{print $1}'
}

snapshot_taken=0
restore() {
  if [ "$snapshot_taken" = 1 ] && [ -f "$SNAPSHOT" ]; then
    cp "$SNAPSHOT" "$TARGET"
    echo "mutation-check: restored $TARGET from the pre-run snapshot" >&2
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
[ "$(needle_count "$TARGET" "$MUTATED")" -eq 0 ] || fail "$TARGET already carries the mutation (a previous interrupted run left it mutated); restore it before running the proof"

mkdir -p "$RUN_ROOT"

echo "== mutation-check: snapshotting the target's current bytes =="
cp "$TARGET" "$SNAPSHOT" || fail "could not snapshot $TARGET; without a snapshot the mutation cannot be undone, so the proof is refused"
snapshot_taken=1
SNAPSHOT_SHA="$(sha_of "$SNAPSHOT")"
[ -s "$SNAPSHOT" ] || fail "the snapshot $SNAPSHOT is empty; refusing to mutate a file it cannot restore"
[ "$(sha_of "$TARGET")" = "$SNAPSHOT_SHA" ] || fail "the snapshot does not byte-match the target it was just taken from"
echo "mutation-check: snapshot sha256=$SNAPSHOT_SHA (a dirty target is expected and allowed)"

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

echo "== mutation-check: restoring the snapshot =="
restore
[ "$(needle_count "$TARGET" "$MUTATED")" -eq 0 ] || fail "the mutated anchor is still present after the revert"
[ "$(sha_of "$TARGET")" = "$SNAPSHOT_SHA" ] || fail "the restored target does not byte-match the pre-run snapshot (expected $SNAPSHOT_SHA, found $(sha_of "$TARGET"))"

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
  "dirty_target_allowed": true,
  "snapshot_sha256": "$SNAPSHOT_SHA",
  "mutated_run_exit": $mutated_status,
  "restored_run_exit": $restored_status,
  "observed_failure": "sign-up: primary button did not reach a signed-in home page",
  "reverted": true,
  "result": "passed"
}
EOF

echo "mutation-check: PASSED — the mutated tree was red for the button, the restored tree was green and byte-identical to the pre-run snapshot (evidence: $RUN_ROOT)"
