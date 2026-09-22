#!/usr/bin/env bash
# Head-tree visual equivalence (T1101 AC3 — the measurement the rework
# letter asked for).
#
# # Why the previous measurement was blind
#
# The first version of this measurement built a control stylesheet out of
# "HEAD's rules ∪ this task's additions" (`control-stylesheet.mjs`) and
# compared the pages against THAT. A rule this task ADDED to a shared
# component exists on both sides of that comparison, so a change living
# inside a new component — a badge border, a label's font size — cancelled to
# exactly zero. It measured what this task PRUNED, not what it REPLACED, and
# it reported 0 pixels for ten pages the real HEAD tree now moves by
# thousands. That file was removed with this rewrite: nothing ran it, and a
# control that cancels the very changes under review is worse than no
# control. (Honest about the other direction: it was the criterion the task
# package named, and it was a weaker criterion than AC3 needs.)
#
# # What this measures instead
#
# The real thing: build the tree as it is at `git archive HEAD`, serve that
# build, render the same seventeen pages at the same viewport with the same
# fixture router, and compare the shots against the CHECKED-IN baselines in
# tests/web-smoke/baseline/ — using the required test's own comparison code,
# so the number this prints is the number that test computes rather than a
# second implementation of it agreeing with itself. The harness runs from the
# worktree (its own baselines, its own current/diff directories); only the
# SERVED BUILD comes from the HEAD copy.
#
# A page inside tolerance means: whatever this task changed, the finished
# page renders like HEAD's did. Seven pages are allowed to exceed it, for three
# different reasons: (1) the four pull-detail tabs share the AC4 radius change
# on `.pull-tab-count`, `.pull-risk-severity` and the other small badges that
# used to be 10px outliers; (2) `project-conflicts` is the req[3] evidence-area
# replacement; (3) `project-overview` and `project-research` are intentional
# design-system fixes — main (T1202 + hotfix/web-demo-regressions) introduced a
# linear-gradient and purple (#8250df) in the new demo surfaces, and T1101
# removes them to satisfy the no-gradient/no-purple policy. For every excepted
# page this script prints the pixel LOCATIONS too — a count alone cannot show
# that the pixels land on the authorized element or the removed violation.
#
# Usage: bash tests/web-smoke/head-tree-visual.sh
# Env:   HEAD_TREE_DIR        scratch tree (default /tmp/t1101-head-tree)
#        HEAD_TREE_PORT       server port (default 31151)
#        HEAD_TREE_REF        git ref to render (default HEAD)
#        HEAD_TREE_REBUILD=1  re-copy and re-build even if the scratch tree is
#                             already the requested ref (HEAD's content does
#                             not move, so the default is to reuse it: the
#                             copy costs a 500MB rsync and a full build)
#        HEAD_TREE_CLEAN=1    delete the scratch tree when the run ends
#
# Sections A/B/E of the shared harness run too — they read the COPY's tree,
# and HEAD's own stylesheets predate this task's radius policy, so their red
# lines are expected here and are not this script's verdict. The verdict
# below is section D, page by page.
set -euo pipefail

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SMOKE_DIR/../.." && pwd)"
WORK="${HEAD_TREE_DIR:-/tmp/t1101-head-tree}"
PORT="${HEAD_TREE_PORT:-31151}"
BASE="http://127.0.0.1:$PORT"
REF="${HEAD_TREE_REF:-HEAD}"
LOG="$WORK-visual.log"

# One source of truth for the tolerance: the harness's own constant.
TOLERANCE="$(grep -oE 'TOLERANCE_PIXELS = [0-9]+' "$SMOKE_DIR/visual-regression.mjs" | grep -oE '[0-9]+')"
EXCEPTIONS=" pull-detail.png pull-detail-scientific.png pull-detail-knowledge.png pull-detail-evidence.png project-conflicts.png project-overview.png project-research.png "
# A page whose HEIGHT moved has no pixel count to report, so the harness
# fails it on the size alone. `project-conflicts` is allowed to differ in
# height because the authorized change there REPLACES one block with another
# (req[3]: the evidence area is the shared Diff, whose two-column body is a
# different height than HEAD's grid). `pull-detail` is not: its authorized
# change (the AC4 radius) is a corner, and a height change there would be an
# unauthorized layout change wearing the exception's name.
SIZE_ALLOWED=" project-conflicts.png "

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

for tool in rsync git tar; do
  command -v "$tool" >/dev/null || { echo "head-tree-visual: $tool is required" >&2; exit 1; }
done
if ss -tln 2>/dev/null | grep -qE ":$PORT[[:space:]]"; then
  echo "head-tree-visual: port $PORT is already in use; a previous run may have leaked a server" >&2
  exit 1
fi

echo "== head-tree-visual: copying the worktree to $WORK (node_modules included) =="
reused=0
if [ "${HEAD_TREE_REBUILD:-0}" != "1" ] && [ -f "$WORK/.head-tree-ref" ] && \
   [ "$(cat "$WORK/.head-tree-ref")" = "$REF" ]; then
  reused=1
  echo "== head-tree-visual: reusing $WORK (already $REF; HEAD_TREE_REBUILD=1 to force a fresh copy) =="
else
  rm -rf "$WORK"
  mkdir -p "$WORK"
  # The copy has to be self-contained: pnpm's node_modules is a lattice of
  # RELATIVE symlinks (apps/web/node_modules/@post/ui -> ../../../../packages/ui),
  # so a copy that keeps the symlinks resolves @post/ui to the COPY's package —
  # which is the point. Excluding .git (a linked worktree's .git is a file
  # pointing into the main checkout, meaningless here), the previous build, and
  # the per-run artifacts.
  rsync -a \
    --exclude '.git' --exclude '.next' --exclude 'shots' \
    --exclude 'current' --exclude 'diff' --exclude 'server.log' \
    "$ROOT/" "$WORK/"

  echo "== head-tree-visual: extracting $REF over the copy =="
  (cd "$ROOT" && git archive "$REF") | tar -x -C "$WORK"
  printf '%s' "$REF" > "$WORK/.head-tree-ref"
fi

# The copy must be the referenced tree and nothing of this task's markup —
# a copy that silently kept this task's pages would render this task's design
# and compare it against this task's baselines, which is the failure mode the
# whole measurement exists to avoid. So it is checked, not assumed.
if ! grep -q "badge-public" "$WORK/apps/web/app/(main)/projects/projects.css"; then
  echo "head-tree-visual: $WORK does not look like $REF (no .badge-public in projects.css)" >&2
  exit 1
fi
if grep -rq "post-state-label" "$WORK/apps/web/app" 2>/dev/null; then
  echo "head-tree-visual: $WORK still carries this task's markup; extracted $REF did not land" >&2
  exit 1
fi
echo "== head-tree-visual: copy verified as $REF (HEAD markup, no shared-component classes) =="

echo "== head-tree-visual: browser library bootstrap (shared web-smoke helper) =="
LIB_PATH="$(bash "$SMOKE_DIR/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

if [ "$reused" = "1" ] && [ -d "$WORK/apps/web/.next" ]; then
  echo "== head-tree-visual: reusing the existing build in $WORK/apps/web/.next =="
else
  echo "== head-tree-visual: building the $REF tree =="
  (
    cd "$WORK/apps/web"
    pnpm run build
  )
fi

echo "== head-tree-visual: starting next on $BASE =="
(
  cd "$WORK/apps/web"
  "$WORK/apps/web/node_modules/.bin/next" start -p "$PORT"
) > "$WORK/server.log" 2>&1 &
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
  echo "head-tree-visual: server did not become ready; log:" >&2
  cat "$WORK/server.log" >&2
  exit 1
fi
echo "== head-tree-visual: server ready =="

echo "== head-tree-visual: render $REF + compare against the checked-in baselines =="
node "$SMOKE_DIR/visual-regression.mjs" "$BASE" "$WORK" > "$LOG" 2>&1 || true

# ---- the verdict, page by page -------------------------------------------
# Parsed out of the shared harness's own output rather than recomputed, so
# "3577 pixels" here and "3577 pixels" in a reviewer's run are the same
# number produced the same way.
awk -v tol="$TOLERANCE" -v exceptions="$EXCEPTIONS" '
  /^(ok|FAIL)[[:space:]]+visual regression / {
    rest = $0
    verdict = (substr($0, 1, 4) == "FAIL") ? "over" : "ok"
    sub(/^(ok|FAIL)[[:space:]]+visual regression /, "", rest)
    # `ok   visual regression x.png — 12 pixel(s) differ, …` and
    # `FAIL visual regression x.png: 12 pixel(s) differ, …` — the separator
    # differs, the count is the same sentence either way.
    name = rest
    sub(/:.*$/, "", name)
    sub(/[[:space:]].*$/, "", name)
    if (rest ~ /page size changed/) { printf "%s\t%s\t%s\n", name, "SIZE", "-"; next }
    if (match(rest, /[0-9]+ pixel\(s\) differ/)) {
      num = substr(rest, RSTART, RLENGTH)
      sub(/ pixel.*$/, "", num)
      printf "%s\t%s\t%s\n", name, verdict, num
      next
    }
    printf "%s\t%s\t%s\n", name, "OTHER", "-"
  }
' "$LOG" > "$WORK-visual.tsv"

echo
echo "page                      differing px   tolerance   verdict"
echo "------------------------- ------------  ----------  ------------------------------------"
# A crashed harness prints no comparison line at all — and "no line" must
# never read as "no difference". First run of this script did exactly that:
# the browser failed to launch, the table came out empty, and the run signed
# off on it. So the row count is checked against the page count before any
# verdict is read.
# Scoped to the PAGES array: `{ file: …` is also the shape of the CSS-policy
# rule list (visual-regression.mjs section A), so a whole-file count asks for
# one comparison line more than there are pages and reads a complete run as a
# crashed one. It did exactly that before this line was scoped.
EXPECTED_PAGES="$(sed -n '/^const PAGES = \[/,$p' "$SMOKE_DIR/visual-regression.mjs" \
  | sed -n '1,/^\];/p' | grep -cE '^\s*\{ file: "')"
ROWS="$(wc -l < "$WORK-visual.tsv")"
bad=0
if [ "$ROWS" -lt "$EXPECTED_PAGES" ]; then
  echo "head-tree-visual: only $ROWS of $EXPECTED_PAGES page(s) reported a comparison;" >&2
  echo "the harness did not finish — its output (tail) follows:" >&2
  tail -25 "$LOG" >&2
  bad=1
else
while IFS=$'\t' read -r name verdict num; do
  page="${name%.png}"
  if [ "$verdict" = "ok" ]; then
    note="within tolerance"
  elif [ "$verdict" = "SIZE" ]; then
    if [[ "$SIZE_ALLOWED" == *" $name "* ]]; then
      note="authorized exception: page height moved (measured below)"
    else
      note="PAGE SIZE CHANGED"; bad=1
    fi
  elif [ "$verdict" = "OTHER" ]; then
    note="no count in the harness output (see $LOG)"; bad=1
  elif [[ "$EXCEPTIONS" == *" $name "* ]]; then
    note="authorized exception (measured below)"
  else
    note="OVER TOLERANCE"; bad=1
  fi
  printf "%-25s %12s  %10s  %s\n" "$page" "$num" "$TOLERANCE" "$note"
done < "$WORK-visual.tsv"
fi

# ---- where the exception pixels land -------------------------------------
# The script promised to show locations for every excepted page, not just the
# two most visually dramatic ones. Loop the EXCEPTIONS list; skip pages that
# did not render a current shot, and skip pages that are identical to baseline.
echo
for name in $EXCEPTIONS; do
  base="$SMOKE_DIR/baseline/$name"
  cur="$SMOKE_DIR/current/$name"
  [ -f "$base" ] && [ -f "$cur" ] || continue
  if cmp -s "$base" "$cur"; then
    echo "-- $name: identical to the baseline (no pixel moved)"; echo
    continue
  fi
  echo "-- $name: pixel locations"
  node "$SMOKE_DIR/diff-locations.mjs" "$base" "$cur" 12 || true
  echo
done

if [ "$bad" != 0 ]; then
  echo "head-tree-visual: FAILED — a page outside the two exceptions moved (harness log: $LOG)" >&2
  [ "${HEAD_TREE_CLEAN:-0}" = "1" ] && rm -rf "$WORK"
  exit 1
fi
echo "head-tree-visual: every page renders like $REF, except the authorized exceptions listed above"
[ "${HEAD_TREE_CLEAN:-0}" = "1" ] && rm -rf "$WORK"
exit 0
