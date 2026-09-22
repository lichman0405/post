#!/usr/bin/env bash
# Style parity between the HEAD tree and the worktree (T1101 AC3 diagnostic).
#
# Serves both trees at once — the HEAD copy that head-tree-visual.sh keeps in
# HEAD_TREE_DIR, and the worktree itself — and diffs every classed element's
# box and type metrics. See style-parity.mjs for what it answers and why it
# is a diagnostic rather than the acceptance measurement.
#
# Usage: bash tests/web-smoke/style-parity.sh [page-substring]
# Env:   HEAD_TREE_DIR (default /tmp/t1101-head-tree), HEAD_TREE_PORT (31151),
#        PARITY_PORT (31150), HEAD_TREE_REBUILD=1 to rebuild the HEAD copy.
set -euo pipefail

SMOKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SMOKE_DIR/../.." && pwd)"
WORK="${HEAD_TREE_DIR:-/tmp/t1101-head-tree}"
HEAD_PORT="${HEAD_TREE_PORT:-31151}"
WORK_PORT="${PARITY_PORT:-31150}"

export POST_ENV=prod
export API_BASE_URL=http://api.e2e.test
export SCIENTIFIC_ADAPTER_URL=http://127.0.0.1:19100

LIB_PATH="$(bash "$SMOKE_DIR/bootstrap-deps.sh")"
export LD_LIBRARY_PATH="${LIB_PATH:+$LIB_PATH:}${LD_LIBRARY_PATH:-}"
export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1

if [ ! -d "$WORK/apps/web/.next" ] || [ "${HEAD_TREE_REBUILD:-0}" = "1" ]; then
  # head-tree-visual.sh builds the copy and then compares it; the comparison
  # may well be red (that is what this diagnostic is for), so its exit code
  # is not this script's.
  echo "== style-parity: building the HEAD copy (head-tree-visual.sh) =="
  bash "$SMOKE_DIR/head-tree-visual.sh" || true
fi
[ -d "$WORK/apps/web/.next" ] || { echo "style-parity: no HEAD build in $WORK" >&2; exit 1; }

for port in "$HEAD_PORT" "$WORK_PORT"; do
  if ss -tln 2>/dev/null | grep -qE ":$port[[:space:]]"; then
    echo "style-parity: port $port is already in use" >&2
    exit 1
  fi
done

echo "== style-parity: building the worktree =="
if [ "${PARITY_SKIP_BUILD:-0}" = "1" ] && [ -d "$ROOT/apps/web/.next" ]; then
  echo "== style-parity: PARITY_SKIP_BUILD=1 — serving the existing build =="
else
  (cd "$ROOT/apps/web" && pnpm run build)
fi

PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] || continue
    pkill -P "$pid" 2>/dev/null || true
    kill "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

echo "== style-parity: serving HEAD on $HEAD_PORT and the worktree on $WORK_PORT =="
(cd "$WORK/apps/web" && "$WORK/apps/web/node_modules/.bin/next" start -p "$HEAD_PORT") > "$SMOKE_DIR/style-parity-head.log" 2>&1 &
PIDS+=("$!")
(cd "$ROOT/apps/web" && "$ROOT/apps/web/node_modules/.bin/next" start -p "$WORK_PORT") > "$SMOKE_DIR/style-parity-work.log" 2>&1 &
PIDS+=("$!")

for url in "http://127.0.0.1:$HEAD_PORT" "http://127.0.0.1:$WORK_PORT"; do
  ready=0
  for _ in $(seq 1 60); do
    if curl -sf -o /dev/null "$url/"; then ready=1; break; fi
    sleep 0.5
  done
  if [ "$ready" != 1 ]; then
    echo "style-parity: $url never became ready" >&2
    exit 1
  fi
done

node "$SMOKE_DIR/style-parity.mjs" "http://127.0.0.1:$HEAD_PORT" "http://127.0.0.1:$WORK_PORT" "${1:-}"
