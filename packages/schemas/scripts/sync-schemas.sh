#!/usr/bin/env bash
#
# sync-schemas.sh — copy the canonical JSON Schemas from specs/schemas/ into
# packages/schemas/schemas/ (published JSON assets) and
# internal/rsg/schemareg/schemas/ (embedded into the Go schema registry).
#
# Canonical source of truth: specs/schemas/ (docs/65). Both copies are synced
# artefacts and must never be hand-edited; check-schema-drift.sh fails the
# build when either diverges.
#
# Deterministic: same inputs -> same bytes out. Run from anywhere in the repo
# (paths are resolved relative to this script).
set -euo pipefail
export LC_ALL=C

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
readonly SRC="$REPO_ROOT/specs/schemas"
readonly DST="$REPO_ROOT/packages/schemas/schemas"
readonly DST_GO="$REPO_ROOT/internal/rsg/schemareg/schemas"

[[ -d "$SRC" ]] || { echo "error: canonical schema dir not found: $SRC" >&2; exit 1; }

mkdir -p "$DST" "$DST_GO"
cp "$SRC"/*.json "$DST"/
cp "$SRC"/*.json "$DST_GO"/

echo "synced $(find "$SRC" -maxdepth 1 -name '*.json' | wc -l) schemas from specs/schemas -> packages/schemas/schemas, internal/rsg/schemareg/schemas"
