#!/usr/bin/env bash
#
# check-schema-drift.sh — fail when either synced copy of the canonical
# schemas (packages/schemas/schemas/ and internal/rsg/schemareg/schemas/)
# diverges from specs/schemas/. Guards against a second, silently-diverging
# schema source of truth (docs/65, T0002 requirement).
#
# Exit codes: 0 = in sync, 1 = drift detected (re-run 'make sync-schemas').
set -uo pipefail
export LC_ALL=C

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
readonly SRC="$REPO_ROOT/specs/schemas"
readonly DSTS=(
  "$REPO_ROOT/packages/schemas/schemas"
  "$REPO_ROOT/internal/rsg/schemareg/schemas"
)

rc=0
for dst in "${DSTS[@]}"; do
  if [[ ! -d "$dst" ]]; then
    echo "schema drift: $dst missing — run 'make sync-schemas'" >&2
    rc=1
    continue
  fi
  if diff -r "$SRC" "$dst" >/dev/null; then
    echo "schemas in sync (specs/schemas == ${dst#"$REPO_ROOT/"})"
  else
    echo "schema drift detected: ${dst#"$REPO_ROOT/"} differs from specs/schemas" >&2
    echo "fix: run 'make sync-schemas' — do not hand-edit the copied schemas" >&2
    rc=1
  fi
done
exit "$rc"
