#!/usr/bin/env bash
# Mutation check for the visual-regression harness (T1101 required test
# "visual regression"): prove the comparison FAILS — on the pixel assertion,
# with a pixel count, on pages that really moved — when the design system is
# broken, and passes again once it is not.
#
# This is the harness's whole reason to exist. Before it, the only screenshot
# machinery in the repo (tests/web-smoke/visual-smoke.mjs) wrote images as
# ARTIFACTS: nothing read them back, so no CSS change could ever turn it red.
# "The screenshots are taken" and "a design regression is caught" are
# different claims, and only the second one is worth a Gate.
#
# Two mutations, because one breakage class is not the other:
#
#   statelabel   the shared StateLabel loses its Primer density (padding
#                1px 8px → 8px 16px). Deliberately LEGAL under section A — no
#                gradient, no glass, no blur, no forbidden hex, no radius
#                change, no shadow — so the static CSS policy stays green and
#                the pixel comparison is the only thing that can catch it.
#
#   fileslayout  `.files-layout` keeps its rule and loses its columns
#                (`grid-template-columns`/`gap`/`align-items` deleted, so the
#                rule still exists and still parses). This is the defect a
#                same-tree baseline is blind to: every className still has a
#                rule, so class-liveness stays green and section A has nothing
#                to say; the three-column Repository Files page simply renders
#                as one stacked column. If the comparison cannot see this, it
#                cannot see the layout it exists to protect.
#
# Each mutation is applied to real product CSS compiled by the real build — not
# to the harness, not to the baseline. Each is reverted here, in this script's
# own trap, and the script refuses to finish without the restored run passing.
#
# Usage: bash tests/web-smoke/visual-regression-mutation-check.sh
#        MUTATIONS="fileslayout" bash tests/web-smoke/visual-regression-mutation-check.sh
#   Runs visual-regression-run.sh twice per mutation (mutated, then a final
#   restored run); each run builds the app, serves it and compares 14 pages.
set -euo pipefail

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SMOKE_DIR/../.." && pwd)"
MUTATIONS="${MUTATIONS:-statelabel fileslayout}"

# ---- mutation 1: the shared StateLabel loses its density ---------------------
#
# A per-rule anchor, not a single line: `border-radius: 999px;` alone occurs
# three times in that file, and a one-line anchor that matches a rule the
# tones override would mutate nothing a reader can see. The selector prefix
# makes it unique — and the script asserts that uniqueness rather than
# assuming it.
STATE_TARGET="$ROOT/packages/ui/src/ui.css"
STATE_ORIGINAL='.post-state-label-badge {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 1px 8px;'
STATE_MUTATED='.post-state-label-badge {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 8px 16px;'
# The pages this mutation is guaranteed to move: the project shell renders the
# Public / Frozen main / repository pending StateLabels on every covered
# project route. Naming pages rather than accepting "some failure" is what
# makes this proof instead of noise.
STATE_PAGES=(projects-directory project-overview project-pulls)

# ---- mutation 2: the Files page loses its three columns ----------------------
FILES_TARGET="$ROOT/apps/web/app/(main)/projects/projects.css"
FILES_ORIGINAL='.files-layout {
  display: grid;
  grid-template-columns: 240px minmax(0, 1fr) 280px;
  gap: 12px;
  align-items: start;
}'
FILES_MUTATED='.files-layout {
  display: grid;
}'
# Exactly this one page: the rule is used by `files/page.tsx` alone, so a
# failure anywhere else would mean the comparison is moving pages it should
# not, and a pass would mean it cannot see a layout regression at all.
FILES_PAGES=(project-files)
FILES_EXACT=1

# Exact, newline-safe presence counting. `grep -F` with a multi-line pattern
# treats each line as a separate pattern, so `grep -qF` would answer "found"
# for a fragment and silently skip the revert — which is exactly the bug this
# script shipped with the first time it ran.
count_occurrences() {
  PAT="$2" perl -0ne '$c += () = /\Q$ENV{PAT}\E/g; END { print $c + 0 }' "$1"
}

substitute() {
  FROM="$2" TO="$3" perl -0pi -e 's/\Q$ENV{FROM}\E/$ENV{TO}/' "$1"
}

restore() {
  if [ "$(count_occurrences "$STATE_TARGET" "$STATE_MUTATED")" -gt 0 ]; then
    substitute "$STATE_TARGET" "$STATE_MUTATED" "$STATE_ORIGINAL"
    echo "== mutation-check: restored packages/ui/src/ui.css ==" >&2
  fi
  if [ "$(count_occurrences "$FILES_TARGET" "$FILES_MUTATED")" -gt 0 ]; then
    substitute "$FILES_TARGET" "$FILES_MUTATED" "$FILES_ORIGINAL"
    echo "== mutation-check: restored apps/web/app/(main)/projects/projects.css ==" >&2
  fi
}
# Also on a signal: this script leaves real product CSS mutated while it runs,
# so an interrupted run must not leave it that way either.
trap restore EXIT INT TERM

arm() { # name target original mutated
  if [ "$(count_occurrences "$2" "$3")" -ne 1 ]; then
    echo "mutation-check: expected exactly one copy of the $1 anchor in $2," >&2
    echo "  found $(count_occurrences "$2" "$3"). Another task may have edited it." >&2
    echo "  Update this script's anchor rather than skipping the check." >&2
    exit 1
  fi
  echo "== mutation-check: arming the $1 mutation =="
  substitute "$2" "$3" "$4"
  if [ "$(count_occurrences "$2" "$4")" -ne 1 ]; then
    echo "mutation-check: the $1 mutation was not armed; refusing to draw a conclusion" >&2
    exit 1
  fi
}

check() { # name target original mutated pages... ; env EXACT=1 for an exact page set
  local name="$1" target="$2" original="$3" mutated="$4"
  shift 4
  local pages=("$@")

  echo "--- $name: mutated run (expect FAIL on the pixel comparison) ---"
  set +e
  mutated_out="$(bash "$SMOKE_DIR/visual-regression-run.sh" 2>&1)"
  mutated_status=$?
  set -e
  echo "$mutated_out" | grep -E "^FAIL|^ok   visual regression|^visual regression" | head -30
  echo "mutation-check: visual-regression-run.sh exited $mutated_status (0 would be the bug)"

  if [ "$mutated_status" -eq 0 ]; then
    echo "mutation-check: BUG — the harness passed while $name was broken" >&2
    exit 1
  fi
  local seen=0
  for page in "${pages[@]}"; do
    # A failure on either the pixel count or a page-size change counts: the
    # statelabel mutation is a padding change, and a page whose only badge is
    # near the bottom can grow by a few pixels rather than showing a counted
    # diff. What matters is that the named pages genuinely moved.
    if grep -qE "^FAIL visual regression ${page}\.png: ([0-9]+ pixel\(s\) differ|page size changed)" <<<"$mutated_out"; then
      seen=$((seen + 1))
    else
      echo "mutation-check: BUG — the harness failed, but not on the pixel/size assertion for ${page}.png" >&2
      exit 1
    fi
  done
  if [ "${EXACT:-0}" = "1" ]; then
    local moved
    moved="$(grep -cE "^FAIL visual regression .*\.png: ([0-9]+ pixel\(s\) differ|page size changed)" <<<"$mutated_out" || true)"
    if [ "$moved" -ne "${#pages[@]}" ]; then
      echo "mutation-check: BUG — the $name mutation moved $moved page(s); expected exactly ${#pages[@]} (${pages[*]})" >&2
      exit 1
    fi
    echo "mutation-check: the comparison caught $name, counted the moved pixels, and moved no page it should not"
  else
    echo "mutation-check: the comparison caught $name and counted the moved pixels ($seen/${#pages[@]} named pages)"
  fi

  echo "--- $name: restoring ---"
  restore
  if [ "$(count_occurrences "$target" "$original")" -ne 1 ]; then
    echo "mutation-check: BUG — the revert did not take; $target is left mutated" >&2
    exit 1
  fi
  rm -f "$SMOKE_DIR"/diff/*.png
}

for name in $MUTATIONS; do
  case "$name" in
    statelabel)
      arm statelabel "$STATE_TARGET" "$STATE_ORIGINAL" "$STATE_MUTATED"
      EXACT=0 check statelabel "$STATE_TARGET" "$STATE_ORIGINAL" "$STATE_MUTATED" "${STATE_PAGES[@]}"
      ;;
    fileslayout)
      arm fileslayout "$FILES_TARGET" "$FILES_ORIGINAL" "$FILES_MUTATED"
      EXACT="$FILES_EXACT" check fileslayout "$FILES_TARGET" "$FILES_ORIGINAL" "$FILES_MUTATED" "${FILES_PAGES[@]}"
      ;;
    *)
      echo "mutation-check: unknown mutation '$name' (known: statelabel fileslayout)" >&2
      exit 1
      ;;
  esac
done

# A run against the restored tree must pass; without it a red baseline would
# make every mutation above look "caught" when nothing was being measured.
echo "--- restored run (expect all pass) ---"
if ! bash "$SMOKE_DIR/visual-regression-run.sh"; then
  echo "mutation-check: BUG — the harness fails against the unmutated tree" >&2
  exit 1
fi
echo "mutation-check: the restored tree passes; the harness is real"
