#!/usr/bin/env bash
#
# check-schema-drift.sh — fail when packages/schemas/schemas/ diverges from the
# canonical specs/schemas/. Guards against a second, silently-diverging schema
# source of truth (docs/65, T0002 requirement).
#
# Exit codes: 0 = in sync, 1 = drift detected (re-run 'make sync-schemas').
set -uo pipefail
export LC_ALL=C

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
readonly SRC="$REPO_ROOT/specs/schemas"
readonly DST="$REPO_ROOT/packages/schemas/schemas"

if [[ ! -d "$DST" ]]; then
  echo "schema drift: $DST missing — run 'make sync-schemas'" >&2
  exit 1
fi

if diff -r "$SRC" "$DST" >/dev/null; then
  echo "schemas in sync (specs/schemas == packages/schemas/schemas)"
  exit 0
fi

echo "schema drift detected: packages/schemas/schemas differs from specs/schemas" >&2
echo "fix: run 'make sync-schemas' — do not hand-edit the copied schemas" >&2
exit 1
